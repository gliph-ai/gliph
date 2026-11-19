//! End-to-end encryption. All of Gliph's cryptography lives here, once.
//!
//! This used to exist twice — TypeScript for the web and desktop, Dart for the
//! phone — and the two could drift by one byte without anything throwing: the
//! symptom is a device that silently cannot read its own channels. This module
//! is the reason that class of bug is now impossible to reintroduce.
//!
//! The wire is unchanged and is not ours to change unilaterally, because a
//! browser tab and a phone have to open each other's blobs:
//!
//!   - every device holds a long-lived P-256 ECDH keypair;
//!   - a channel has a random 256-bit AES-GCM key per epoch;
//!   - to share it, the sender does ECDH-ES to each member device: ephemeral
//!     keypair -> ECDH -> HKDF-SHA256 -> AES-GCM, and seals the channel key;
//!   - messages are AES-256-GCM under the channel key, with the epoch sent in
//!     the clear so recipients know which key to use.
//!
//! The server sees: public keys, sealed blobs, ciphertext, and an epoch number.
//! It cannot read a message even with the whole database.
//!
//! ponytail: sender keys, not MLS — no per-message forward secrecy and no
//! post-compromise security. Removing a member rotates the epoch, which
//! protects future traffic, but a stolen device key opens everything that
//! device could already read. MLS is the upgrade path, it is Rust (OpenMLS),
//! and it now has exactly one place to land instead of three.

use std::collections::BTreeMap;

use aes_gcm::aead::{Aead, KeyInit, Payload};
use aes_gcm::Aes256Gcm;
use base64::engine::general_purpose::STANDARD as B64;
use base64::Engine;
use hkdf::Hkdf;
use p256::ecdh::diffie_hellman;
use p256::ecdsa::signature::{Signer, Verifier};
use p256::ecdsa::{Signature, SigningKey, VerifyingKey};
use p256::pkcs8::{DecodePublicKey, EncodePublicKey};
use p256::{PublicKey, SecretKey};
use rand_core::{OsRng, RngCore};
use sha2::Sha256;

use crate::wire::DeviceKey;

const WRAP_INFO: &str = "gliph/e2ee/v1/wrap";
/// HKDF info for the account master key travelling to a newly approved device.
pub const MASTER_INFO: &str = "gliph/e2ee/v1/master";
const DEVICE_STATEMENT: &str = "gliph/device/v1";
const IV_LEN: usize = 12;

#[derive(Debug)]
pub enum CryptoError {
    BadKey,
    NoKey,
    Failed,
}

pub type Result<T> = std::result::Result<T, CryptoError>;

pub fn random_bytes(n: usize) -> Vec<u8> {
    let mut v = vec![0u8; n];
    OsRng.fill_bytes(&mut v);
    v
}

/// A v4 UUID from the same CSPRNG everything else here uses. The `uuid` crate
/// wants a feature flag per platform to find a source of randomness on wasm,
/// which is a dependency and a build-configuration trap for sixteen bytes and
/// two bit-masks.
pub fn uuid4() -> String {
    let mut b = random_bytes(16);
    b[6] = (b[6] & 0x0f) | 0x40; // version 4
    b[8] = (b[8] & 0x3f) | 0x80; // variant 1
    let h = |r: &[u8]| -> String { r.iter().map(|x| format!("{x:02x}")).collect() };
    format!("{}-{}-{}-{}-{}", h(&b[0..4]), h(&b[4..6]), h(&b[6..8]), h(&b[8..10]), h(&b[10..16]))
}

// ---- device identity ----

/// This device's long-lived keypair. The secret is handed to the host to
/// persist (Keychain on iOS, IndexedDB on the web) and handed back on the next
/// launch — the core never touches storage itself.
#[derive(Clone)]
pub struct Identity {
    pub id: String,
    secret: SecretKey,
}

impl Identity {
    pub fn generate() -> Identity {
        Identity { id: uuid4(), secret: SecretKey::random(&mut OsRng) }
    }

    /// Rebuild from what `secret_hex` handed out last time.
    pub fn restore(id: &str, secret_hex: &str) -> Result<Identity> {
        let bytes = hex_decode(secret_hex).ok_or(CryptoError::BadKey)?;
        let secret = SecretKey::from_slice(&bytes).map_err(|_| CryptoError::BadKey)?;
        Ok(Identity { id: id.to_string(), secret })
    }

    /// The private scalar, for the host to store. This is the one secret in the
    /// product that must never reach a server.
    pub fn secret_hex(&self) -> String {
        hex_encode(&self.secret.to_bytes())
    }

    /// Base64 SPKI, exactly what WebCrypto's `exportKey("spki")` produces —
    /// this is what goes on the wire in `device.hello`.
    pub fn pubkey(&self) -> String {
        let der = self.secret.public_key().to_public_key_der().expect("p256 spki");
        B64.encode(der.as_bytes())
    }
}

fn hex_encode(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

fn hex_decode(s: &str) -> Option<Vec<u8>> {
    if s.len() % 2 != 0 {
        return None;
    }
    (0..s.len()).step_by(2).map(|i| u8::from_str_radix(&s[i..i + 2], 16).ok()).collect()
}

// ---- primitives ----

/// ECDH -> HKDF -> an AES-GCM key used only to seal/open a channel key.
fn wrap_key_for(secret: &SecretKey, peer: &PublicKey, info: &str) -> [u8; 32] {
    let shared = diffie_hellman(secret.to_nonzero_scalar(), peer.as_affine());
    // WebCrypto is handed a zero-length salt, which HKDF-Extract treats as
    // HashLen zero bytes — which is what `None` means here.
    let hk = Hkdf::<Sha256>::new(None, shared.raw_secret_bytes());
    let mut out = [0u8; 32];
    hk.expand(info.as_bytes(), &mut out).expect("32 bytes is a valid HKDF length");
    out
}

fn seal(key: &[u8; 32], iv: &[u8], plain: &[u8]) -> Result<Vec<u8>> {
    Aes256Gcm::new(key.into())
        .encrypt(iv.into(), Payload { msg: plain, aad: &[] })
        .map_err(|_| CryptoError::Failed)
}

fn open(key: &[u8; 32], iv: &[u8], ct: &[u8]) -> Result<Vec<u8>> {
    Aes256Gcm::new(key.into())
        .decrypt(iv.into(), Payload { msg: ct, aad: &[] })
        .map_err(|_| CryptoError::Failed)
}

/// Bind a channel key's wrapping to its channel and epoch, so a blob resealed
/// elsewhere cannot be replayed into another room.
fn channel_info(channel: &str, epoch: i64) -> String {
    format!("{WRAP_INFO}/{channel}/{epoch}")
}

/// ECDH-ES to one device: a fresh ephemeral pair, so this device's long-term
/// key is never used to encrypt and cannot be correlated across rooms.
///
/// blob = 2-byte ephemeral-pubkey length ‖ ephemeral pubkey ‖ iv ‖ sealed
pub fn seal_for(peer_pubkey_b64: &str, info: &str, plain: &[u8]) -> Option<String> {
    let pub_der = B64.decode(peer_pubkey_b64).ok()?;
    let peer_pub = PublicKey::from_public_key_der(&pub_der).ok()?;
    let eph = SecretKey::random(&mut OsRng);
    let wrapper = wrap_key_for(&eph, &peer_pub, info);
    let iv = random_bytes(IV_LEN);
    let sealed = seal(&wrapper, &iv, plain).ok()?;
    let eph_der = eph.public_key().to_public_key_der().ok()?;
    let eph_pub = eph_der.as_bytes();
    let mut blob = Vec::with_capacity(2 + eph_pub.len() + IV_LEN + sealed.len());
    blob.push((eph_pub.len() >> 8) as u8);
    blob.push((eph_pub.len() & 0xff) as u8);
    blob.extend_from_slice(eph_pub);
    blob.extend_from_slice(&iv);
    blob.extend_from_slice(&sealed);
    Some(B64.encode(blob))
}

impl Identity {
    /// Open a blob `seal_for` addressed to this device.
    pub fn open_for(&self, info: &str, blob_b64: &str) -> Result<Vec<u8>> {
        let blob = B64.decode(blob_b64).map_err(|_| CryptoError::BadKey)?;
        if blob.len() < 2 {
            return Err(CryptoError::BadKey);
        }
        let eph_len = ((blob[0] as usize) << 8) | blob[1] as usize;
        if blob.len() < 2 + eph_len + IV_LEN {
            return Err(CryptoError::BadKey);
        }
        let eph_pub = PublicKey::from_public_key_der(&blob[2..2 + eph_len])
            .map_err(|_| CryptoError::BadKey)?;
        let iv = &blob[2 + eph_len..2 + eph_len + IV_LEN];
        let sealed = &blob[2 + eph_len + IV_LEN..];
        open(&wrap_key_for(&self.secret, &eph_pub, info), iv, sealed)
    }
}

// ---- device signing ----
//
// An account has one P-256 ECDSA master key. Its first device mints it, and it
// signs every device the account owns: `sign(user ‖ device ‖ device pubkey)`.
// A peer seals a channel key only to a device whose signature verifies against
// the owner's master, and the server refuses keys to an unsigned device, so a
// stolen password alone gets a session and nothing it can decrypt.
//
// The master secret travels to each approved device sealed to that device, so
// any signed device can approve the next one. A recovery key is the same move
// with a different carrier: the master secret sealed under the recovery key
// and parked on the server. That is the follow-up; nothing here assumes the
// signer is a device.

/// The account master signing key.
#[derive(Clone)]
pub struct Master(SigningKey);

impl Master {
    pub fn generate() -> Master {
        Master(SigningKey::random(&mut OsRng))
    }

    pub fn restore(secret_hex: &str) -> Result<Master> {
        let bytes = hex_decode(secret_hex).ok_or(CryptoError::BadKey)?;
        SigningKey::from_slice(&bytes).map(Master).map_err(|_| CryptoError::BadKey)
    }

    pub fn secret_hex(&self) -> String {
        hex_encode(&self.0.to_bytes())
    }

    /// Base64 SPKI, the same shape as a device key.
    pub fn pubkey(&self) -> String {
        let pk = PublicKey::from(self.0.verifying_key());
        B64.encode(pk.to_public_key_der().expect("p256 spki").as_bytes())
    }

    /// DER signature, base64. Go's `ecdsa.VerifyASN1` reads exactly this.
    pub fn sign_device(&self, user_id: &str, device_id: &str, pubkey: &str) -> String {
        let sig: Signature = self.0.sign(device_statement(user_id, device_id, pubkey).as_bytes());
        B64.encode(sig.to_der().as_bytes())
    }
}

fn device_statement(user_id: &str, device_id: &str, pubkey: &str) -> String {
    format!("{DEVICE_STATEMENT}\n{user_id}\n{device_id}\n{pubkey}")
}

/// Does this device's signature verify against its owner's master key?
pub fn verify_device(master: &str, user_id: &str, device_id: &str, pubkey: &str, sig: &str) -> bool {
    let (Ok(mder), Ok(sder)) = (B64.decode(master), B64.decode(sig)) else { return false };
    let Ok(pk) = PublicKey::from_public_key_der(&mder) else { return false };
    let Ok(sig) = Signature::from_der(&sder) else { return false };
    VerifyingKey::from(pk).verify(device_statement(user_id, device_id, pubkey).as_bytes(), &sig).is_ok()
}

/// The approval code: the first 48 bits of SHA-256 over the device's SPKI, as
/// `abcd-ef01-2345`. The person reads it off the new device and types it on
/// the old one, which is what stops a server slipping in a device of its own.
/// The server computes the same string for `/devices`.
pub fn fingerprint(pubkey_b64: &str) -> String {
    use sha2::Digest;
    let der = B64.decode(pubkey_b64).unwrap_or_default();
    let h = hex_encode(&Sha256::digest(&der)[..6]);
    format!("{}-{}-{}", &h[0..4], &h[4..8], &h[8..12])
}

// ---- channel keys ----

/// channel -> epoch -> raw key. In memory only: re-fetched (sealed) on launch,
/// which is also what makes a stolen device backup worth nothing on its own.
#[derive(Default)]
pub struct Keyring {
    keys: BTreeMap<(String, i64), [u8; 32]>,
}

impl Keyring {
    pub fn have(&self, channel: &str, epoch: i64) -> bool {
        self.keys.contains_key(&(channel.to_string(), epoch))
    }

    pub fn latest_epoch(&self, channel: &str) -> i64 {
        self.keys.keys().filter(|(c, _)| c == channel).map(|(_, e)| *e).max().unwrap_or(0)
    }

    /// Mint a fresh channel key for a new epoch (channel creation, or re-key
    /// after a membership change).
    pub fn mint(&mut self, channel: &str, epoch: i64) {
        let mut key = [0u8; 32];
        OsRng.fill_bytes(&mut key);
        self.keys.insert((channel.to_string(), epoch), key);
    }

    /// Drop every decrypted key, keeping the device identity. This is what a
    /// relaunch looks like — and what the sealed blobs on the server undo.
    pub fn clear(&mut self) {
        self.keys.clear();
    }

    /// Seal a channel key to each peer device. Returns {device_id: blob} for
    /// `keys.publish`. A device whose public key will not parse is skipped
    /// rather than failing the whole re-key.
    pub fn seal_to(
        &self,
        channel: &str,
        epoch: i64,
        peers: &[DeviceKey],
    ) -> Result<BTreeMap<String, String>> {
        let key = self.keys.get(&(channel.to_string(), epoch)).ok_or(CryptoError::NoKey)?;
        let mut out = BTreeMap::new();
        let info = channel_info(channel, epoch);
        for peer in peers {
            if let Some(blob) = seal_for(&peer.pubkey, &info, key) {
                out.insert(peer.id.clone(), blob);
            }
        }
        Ok(out)
    }

    /// Open a sealed channel key addressed to this device.
    pub fn open_sealed(
        &mut self,
        me: &Identity,
        channel: &str,
        epoch: i64,
        blob_b64: &str,
    ) -> Result<()> {
        let raw = me.open_for(&channel_info(channel, epoch), blob_b64)?;
        if raw.len() != 32 {
            return Err(CryptoError::BadKey);
        }
        let mut key = [0u8; 32];
        key.copy_from_slice(&raw);
        self.keys.insert((channel.to_string(), epoch), key);
        Ok(())
    }

    /// Encrypt a message body. Returns the base64 payload and the epoch used.
    pub fn encrypt_body(&self, channel: &str, text: &str) -> Result<(String, i64)> {
        let epoch = self.latest_epoch(channel);
        let key = self.keys.get(&(channel.to_string(), epoch)).ok_or(CryptoError::NoKey)?;
        let iv = random_bytes(IV_LEN);
        let ct = seal(key, &iv, text.as_bytes())?;
        let mut out = Vec::with_capacity(IV_LEN + ct.len());
        out.extend_from_slice(&iv);
        out.extend_from_slice(&ct);
        Ok((B64.encode(out), epoch))
    }

    /// Decrypt a message body. `None` when this device has no key for that
    /// epoch, so the UI can show an honest "cannot decrypt" line instead of a
    /// lie — and `None` again on a bad tag, so tampered ciphertext is never
    /// rendered as if it were a message.
    pub fn decrypt_body(&self, channel: &str, epoch: i64, body_b64: &str) -> Option<String> {
        let key = self.keys.get(&(channel.to_string(), epoch))?;
        let raw = B64.decode(body_b64).ok()?;
        if raw.len() < IV_LEN {
            return None;
        }
        let plain = open(key, &raw[..IV_LEN], &raw[IV_LEN..]).ok()?;
        String::from_utf8(plain).ok()
    }
}
