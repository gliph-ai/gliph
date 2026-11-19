// The desktop end of "link this device" (see server/devicelink.go for the flow
// and the threat model).
//
// This has to run in Rust rather than in the webview, for a reason that is easy
// to miss: the system browser cannot reach the webview. A bundled Tauri app
// serves its page from a custom scheme, so there is no origin the browser could
// redirect back to — the callback has to be a real socket on a real port, and
// only the native side can bind one.
//
// ponytail: a hand-rolled listener rather than an HTTP crate. The whole server
// is one request: read a line, pull the query off it, write a fixed response.
// Pulling in a web framework to parse `GET /cb?code=… HTTP/1.1` would be more
// dependency than protocol.

use std::io::{BufRead, BufReader, Write};
use std::net::{TcpListener, TcpStream};
use std::time::{Duration, Instant};

use base64::Engine;
use sha2::{Digest, Sha256};
use tauri::AppHandle;

/// How long to wait for the human. Long enough to find the browser window and
/// read the page; short enough that an abandoned attempt releases the port.
const DEADLINE: Duration = Duration::from_secs(180);

fn b64url(bytes: &[u8]) -> String {
    base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(bytes)
}

fn random_b64(len: usize) -> String {
    let mut buf = vec![0u8; len];
    getrandom::fill(&mut buf).expect("no system randomness");
    b64url(&buf)
}

/// Everything the browser is asked to approve, plus the secret it never sees.
struct Pkce {
    verifier: String,
    challenge: String,
    state: String,
}

impl Pkce {
    fn new() -> Self {
        // 64 random bytes. The verifier is the one value in this flow that must
        // never leave the process — it is what proves the app that redeems the
        // code is the app that started the flow.
        let verifier = random_b64(64);
        let challenge = b64url(&Sha256::digest(verifier.as_bytes()));
        Self { verifier, challenge, state: random_b64(16) }
    }
}

/// Read the request line of one HTTP request and return its query string.
fn read_query(stream: &TcpStream) -> Option<String> {
    let mut line = String::new();
    // A client that connects and says nothing must not hold the flow open.
    let _ = stream.set_read_timeout(Some(Duration::from_secs(10)));
    BufReader::new(stream).read_line(&mut line).ok()?;
    // "GET /cb?code=…&state=… HTTP/1.1"
    let path = line.split_whitespace().nth(1)?;
    let (_, query) = path.split_once('?')?;
    Some(query.to_string())
}

fn param(query: &str, key: &str) -> Option<String> {
    query.split('&').find_map(|pair| {
        let (k, v) = pair.split_once('=')?;
        if k != key {
            return None;
        }
        Some(percent_decode(v))
    })
}

/// Enough percent-decoding for the values this callback carries (a base64url
/// code and a state). Anything it cannot decode it passes through, which is the
/// safe direction: a mangled code simply fails to redeem.
fn percent_decode(s: &str) -> String {
    let bytes = s.replace('+', " ").into_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'%' && i + 2 < bytes.len() {
            if let Ok(b) = u8::from_str_radix(&String::from_utf8_lossy(&bytes[i + 1..i + 3]), 16) {
                out.push(b);
                i += 3;
                continue;
            }
        }
        out.push(bytes[i]);
        i += 1;
    }
    String::from_utf8_lossy(&out).into_owned()
}

fn respond(mut stream: TcpStream, title: &str, message: &str) {
    let body = format!(
        "<!doctype html><meta charset=utf-8><title>{title}</title>\
         <body style=\"font:15px/1.6 ui-monospace,monospace;background:#1a2138;color:#f5f1e8;\
         display:flex;align-items:center;justify-content:center;height:100vh;margin:0\">\
         <div style=\"text-align:center\"><h1 style=\"color:#e8517c;margin:0 0 8px\">{title}</h1>\
         <p style=\"color:#9aa3c4;margin:0\">{message}</p></div>"
    );
    let _ = write!(
        stream,
        "HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=utf-8\r\n\
         Content-Length: {}\r\nConnection: close\r\n\r\n{body}",
        body.len()
    );
    let _ = stream.flush();
}

/// Run the whole exchange and return the session token the server minted.
///
/// `server` is the gliph server's base URL; `web` is where the browser-facing
/// client is served from (usually the same host).
#[tauri::command]
pub async fn start_device_link(
    app: AppHandle,
    server: String,
    web: Option<String>,
) -> Result<String, String> {
    let web = web.unwrap_or_else(|| server.clone());
    let pkce = Pkce::new();

    // Port 0: let the OS pick a free one. A fixed port is a port some other
    // program already has.
    let listener = TcpListener::bind("127.0.0.1:0").map_err(|e| e.to_string())?;
    let port = listener.local_addr().map_err(|e| e.to_string())?.port();
    let redirect = format!("http://127.0.0.1:{port}/cb");

    let device_name = hostname();
    let url = format!(
        "{}/link.html?device_name={}&platform=desktop&redirect_uri={}&state={}\
         &code_challenge={}&code_challenge_method=S256",
        web.trim_end_matches('/'),
        urlencode(&device_name),
        urlencode(&redirect),
        urlencode(&pkce.state),
        urlencode(&pkce.challenge),
    );
    tauri_plugin_opener::open_url(&url, None::<&str>).map_err(|e| e.to_string())?;
    let _ = &app;

    // Wait for the callback, refusing anything that is not ours.
    let deadline = Instant::now() + DEADLINE;
    listener
        .set_nonblocking(true)
        .map_err(|e| e.to_string())?;

    let code = loop {
        if Instant::now() > deadline {
            return Err("timed out waiting for approval".into());
        }
        match listener.accept() {
            Ok((stream, _)) => {
                let Some(query) = read_query(&stream) else {
                    respond(stream, "gliph", "That request made no sense.");
                    continue;
                };
                // `state` is the check that this callback belongs to the flow we
                // started, and not to some other process poking at the port.
                if param(&query, "state").as_deref() != Some(pkce.state.as_str()) {
                    respond(stream, "gliph", "That link did not come from this app.");
                    continue;
                }
                if let Some(err) = param(&query, "error") {
                    respond(stream, "Not linked", "You can close this tab.");
                    return Err(format!("link refused: {err}"));
                }
                let Some(code) = param(&query, "code") else {
                    respond(stream, "gliph", "No code came back.");
                    continue;
                };
                respond(stream, "Linked", "You can close this tab and go back to Gliph.");
                break code;
            }
            Err(ref e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                std::thread::sleep(Duration::from_millis(150));
            }
            Err(e) => return Err(e.to_string()),
        }
    };

    // Redeem. The verifier goes over the wire for the first and only time here,
    // to the server that already holds its hash.
    let body = serde_json::json!({
        "code": code,
        "code_verifier": pkce.verifier,
        "redirect_uri": redirect,
    });
    let res = reqwest::Client::new()
        .post(format!("{}/api/auth/device-link/token", server.trim_end_matches('/')))
        .json(&body)
        .send()
        .await
        .map_err(|e| e.to_string())?;
    if !res.status().is_success() {
        return Err(format!("server refused the code ({})", res.status()));
    }
    let parsed: serde_json::Value = res.json().await.map_err(|e| e.to_string())?;
    parsed
        .get("token")
        .and_then(|t| t.as_str())
        .map(str::to_owned)
        .ok_or_else(|| "server returned no token".into())
}

fn urlencode(s: &str) -> String {
    s.bytes()
        .map(|b| match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => {
                (b as char).to_string()
            }
            _ => format!("%{b:02X}"),
        })
        .collect()
}

fn hostname() -> String {
    std::process::Command::new("hostname")
        .output()
        .ok()
        .and_then(|o| String::from_utf8(o.stdout).ok())
        .map(|s| s.trim().to_string())
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| "Gliph desktop".into())
}
