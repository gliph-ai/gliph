//! The check this core cannot go without.
//!
//! crypto.rs is a reimplementation of what web/src/crypto.ts did with WebCrypto,
//! on a different library. If it drifts by one byte — the curve, the HKDF info
//! string, the IV length, a blob offset — nothing throws: devices simply stop
//! being able to read each other's channels, and it looks like a key
//! distribution bug for a week.
//!
//! So the fixture was produced by the SAME WebCrypto calls the browser made,
//! and this opens it with the Rust code. Passing means every client that has
//! ever run can still read every message it could read before the rewrite.

use gliph_core::crypto::{Identity, Keyring};
use gliph_core::wire::DeviceKey;
use serde_json::Value;

fn vector() -> Value {
    serde_json::from_str(include_str!("webcrypto_vector.json")).expect("fixture parses")
}

/// The private scalar out of a P-256 PKCS#8 blob. Every byte before it is fixed
/// for this curve, so this is an offset rather than an ASN.1 parser.
fn private_scalar(pkcs8: &[u8]) -> String {
    assert_eq!(pkcs8.len(), 138, "not a P-256 PKCS#8 key");
    assert_eq!(pkcs8[34], 0x04, "expected the OCTET STRING tag");
    assert_eq!(pkcs8[35], 0x20, "expected a 32-byte scalar");
    pkcs8[36..68].iter().map(|b| format!("{b:02x}")).collect()
}

fn b64(s: &str) -> Vec<u8> {
    use base64::engine::general_purpose::STANDARD as B64;
    use base64::Engine;
    B64.decode(s).expect("base64")
}

#[test]
fn opens_a_channel_key_the_browser_sealed_to_this_device() {
    let v = vector();
    let me = Identity::restore(
        "fixture-device",
        &private_scalar(&b64(v["recipient_pkcs8"].as_str().unwrap())),
    )
    .expect("restore");

    assert_eq!(
        me.pubkey(),
        v["recipient_spki"].as_str().unwrap(),
        "SPKI export must match WebCrypto exportKey(\"spki\")"
    );

    let channel = v["channel"].as_str().unwrap();
    let epoch = v["epoch"].as_i64().unwrap();
    let mut ring = Keyring::default();
    assert!(!ring.have(channel, epoch));

    ring.open_sealed(&me, channel, epoch, v["sealed_blob"].as_str().unwrap())
        .expect("ECDH -> HKDF -> AES-GCM unwrap must agree with WebCrypto");
    assert!(ring.have(channel, epoch));

    // And the key recovered is the one the browser minted, which is what makes
    // the message below readable at all.
    assert_eq!(
        ring.decrypt_body(channel, epoch, v["message_body"].as_str().unwrap()).as_deref(),
        Some(v["message_text"].as_str().unwrap()),
    );
}

#[test]
fn a_key_sealed_to_this_device_opens_with_this_device_key() {
    let me = Identity::generate();
    let mut ring = Keyring::default();
    ring.mint("c_1", 1);
    let sealed = ring.encrypt_body("c_1", "sealed then opened").unwrap();

    let entries = ring
        .seal_to("c_1", 1, &[DeviceKey { id: me.id.clone(), pubkey: me.pubkey(), ..Default::default() }])
        .unwrap();
    assert_eq!(entries.len(), 1);

    // Forget the key without forgetting who we are, and recover it from the
    // blob alone — the path every device that did NOT mint it takes.
    ring.clear();
    assert!(!ring.have("c_1", 1));
    assert_eq!(ring.decrypt_body("c_1", 1, &sealed.0), None);

    ring.open_sealed(&me, "c_1", 1, &entries[&me.id]).unwrap();
    assert_eq!(ring.decrypt_body("c_1", 1, &sealed.0).as_deref(), Some("sealed then opened"));
}

#[test]
fn a_wrong_epoch_stays_unreadable() {
    let mut ring = Keyring::default();
    ring.mint("c_2", 4);
    let text = "ascii art: ┌──┐ and a 🦄";
    let (body, epoch) = ring.encrypt_body("c_2", text).unwrap();
    assert_eq!(epoch, 4);
    assert_eq!(ring.decrypt_body("c_2", 4, &body).as_deref(), Some(text));
    // No key for epoch 5 — an honest None, never garbage.
    assert_eq!(ring.decrypt_body("c_2", 5, &body), None);
}

#[test]
fn a_tampered_ciphertext_is_refused_not_rendered() {
    use base64::engine::general_purpose::STANDARD as B64;
    use base64::Engine;

    let mut ring = Keyring::default();
    ring.mint("c_3", 1);
    let (body, _) = ring.encrypt_body("c_3", "the transfer is approved").unwrap();
    let mut bytes = B64.decode(&body).unwrap();
    let last = bytes.len() - 1;
    bytes[last] ^= 0x01; // flip a bit in the GCM tag
    assert_eq!(ring.decrypt_body("c_3", 1, &B64.encode(bytes)), None);
}

#[test]
fn a_restored_identity_is_the_same_device() {
    let me = Identity::generate();
    let again = Identity::restore(&me.id, &me.secret_hex()).unwrap();
    assert_eq!(me.pubkey(), again.pubkey(), "a relaunch must not mint a new device");
}
