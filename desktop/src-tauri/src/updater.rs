// In-app updates.
//
// The manifest lives at the endpoint in tauri.conf.json and is signed with a
// key whose public half is baked into the binary. That signature is the whole
// security model: a compromised download host can serve any bytes it likes and
// the client will refuse them, because it can only install what the private key
// signed. Nothing here needs to trust the transport.
//
// Two entry points, and deliberately no more: a quiet check shortly after
// launch, and an explicit one from the menu. An updater that interrupts is an
// updater people disable.

use tauri::{AppHandle, Emitter};
use tauri_plugin_updater::UpdaterExt;

/// Look for an update without blocking startup or bothering anyone, and keep
/// looking while the app is open. On a hit it emits `update://available`; the
/// web client decides how loudly to mention it.
///
/// Every push to main is a new build, so a client left open for a day would
/// otherwise never learn about any of them: the poll is what makes the ★ show
/// up on the testers' machines within minutes.
pub fn check_on_startup(app: &AppHandle) {
    let app = app.clone();
    // A plain OS thread for the wait, not the async runtime: sleeping on a
    // runtime worker would park a thread the plugins share.
    std::thread::spawn(move || {
        // Let the window paint and the socket connect first. An update check is
        // never the most urgent thing happening at launch.
        std::thread::sleep(std::time::Duration::from_secs(3));
        loop {
            let app = app.clone();
            tauri::async_runtime::spawn(async move {
                match check(&app).await {
                    Ok(Some(version)) => {
                        log::info!("update available: {version}");
                        let _ = app.emit("update://available", version);
                    }
                    Ok(None) => log::debug!("no update available"),
                    // Offline, or the endpoint is not serving a manifest yet. Neither is
                    // worth a word to the user.
                    Err(e) => log::debug!("update check failed: {e}"),
                }
            });
            // Two minutes: a push lands as a ★ before anyone has to ask. One
            // small GET against our own server; nothing to economise on.
            std::thread::sleep(std::time::Duration::from_secs(2 * 60));
        }
    });
}

/// The plugin's default client waits forever, so a download that stalls left
/// the ★ busy until the app was quit. A deadline turns a stall into an error
/// the web side reports, with the ★ live again to retry.
fn updater(app: &AppHandle, secs: u64) -> Result<tauri_plugin_updater::Updater, String> {
    app.updater_builder()
        .timeout(std::time::Duration::from_secs(secs))
        .build()
        .map_err(|e| e.to_string())
}

async fn check(app: &AppHandle) -> Result<Option<String>, String> {
    // Without the plugin, `app.updater()` reaches for state that was never
    // managed and panics — inside an async command that is a promise that
    // never settles, which the web side shows as "checking…" forever.
    if !updates_supported(app) {
        return Err("this build has no updater (a local bin/install-app build); install it from https://gliph.sh/downloads".into());
    }
    let found = updater(app, 30)?.check().await.map_err(|e| e.to_string())?;
    Ok(found.map(|u| u.version))
}

/// Explicit "check for updates", from the app menu. Unlike the startup check
/// this one is allowed to say "you are up to date", because a person asked.
pub fn check_now(app: &AppHandle) {
    let app = app.clone();
    tauri::async_runtime::spawn(async move {
        match check(&app).await {
            Ok(Some(version)) => {
                let _ = app.emit("update://available", version);
            }
            Ok(None) => {
                let _ = app.emit("update://none", ());
            }
            Err(e) => {
                let _ = app.emit("update://error", e);
            }
        }
    });
}

/// Download and install, streaming progress as `update://progress` (bytes so
/// far, total when the server declared one), then relaunch.
#[tauri::command]
pub async fn install_update(app: AppHandle) -> Result<(), String> {
    if !updates_supported(&app) {
        return Err("this build has no updater".into());
    }
    // Ten minutes covers a slow hotel wifi; the timeout is the whole request,
    // body included, not a gap between bytes.
    let update = updater(&app, 10 * 60)?
        .check()
        .await
        .map_err(|e| e.to_string())?
        .ok_or_else(|| "no update available".to_string())?;

    let mut downloaded: usize = 0;
    let progress_app = app.clone();
    let done_app = app.clone();
    update
        .download_and_install(
            move |chunk, total| {
                downloaded += chunk;
                let _ = progress_app.emit("update://progress", (downloaded, total));
            },
            // The last chunk's event can lose the race with the restart; say
            // "all of it" once, explicitly, so the menubar reads restarting.
            move || {
                let _ = done_app.emit("update://progress", (1usize, Some(1u64)));
            },
        )
        .await
        .map_err(|e| e.to_string())?;

    // On macOS and Windows this replaces the app and comes back. On Linux .deb
    // and .rpm the install hands off to a privileged system prompt, and an
    // AppImage only updates when the user is actually running the AppImage.
    app.restart();
}

/// Ask the updater whether one is waiting, from the web side.
#[tauri::command]
pub async fn check_for_updates(app: AppHandle) -> Result<Option<String>, String> {
    check(&app).await
}

/// True when this build can update itself at all. A dev build has updater
/// artifacts disabled, so offering the menu item there is a lie.
pub fn updates_supported(app: &AppHandle) -> bool {
    // The same condition lib.rs registers the plugin on. Asking the config
    // rather than the identifier keeps the menu honest for every build that has
    // no updater — a dev build, but also anything built without the release
    // config — instead of only the one whose name we happened to check for.
    app.config().plugins.0.contains_key("updater")
}
