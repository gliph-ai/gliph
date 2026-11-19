// The web client talks to the Go server directly over HTTP/WebSocket, so the
// Rust side only hosts the window, keeps its geometry, and routes external
// links out to the browser.
//
// The window-state, exit and link-routing behaviour here follows what the
// inbox2 desktop app learned the hard way; the comments explain why each one is
// not the obvious code.

mod devicelink;
mod mesh;
mod notifypanel;
mod updater;

use tauri::{Emitter, Manager};

/// Panics inside spawned tasks only reach stderr, which is /dev/null when the
/// app is launched from Finder. Route them to the log file instead.
fn install_panic_hook() {
    let default_hook = std::panic::take_hook();
    std::panic::set_hook(Box::new(move |info| {
        let location = info
            .location()
            .map(|l| format!("{}:{}:{}", l.file(), l.line(), l.column()))
            .unwrap_or_else(|| "unknown location".into());
        let msg = info
            .payload()
            .downcast_ref::<&str>()
            .map(|s| s.to_string())
            .or_else(|| info.payload().downcast_ref::<String>().cloned())
            .unwrap_or_else(|| "<non-string panic payload>".into());
        log::error!("PANIC at {location}: {msg}");
        default_hook(info);
    }));
}

/// Log a message from the JS webview into the Rust log file. The webview's own
/// console is gone the moment the app is launched from Finder, so anything the
/// client wants to survive a crash has to come through here.
#[tauri::command]
async fn js_log(level: String, message: String) {
    match level.as_str() {
        "error" => log::error!("[JS] {message}"),
        "warn" => log::warn!("[JS] {message}"),
        "debug" => log::debug!("[JS] {message}"),
        _ => log::info!("[JS] {message}"),
    }
}

/// Custom app menu. ⌘Q stays the native Quit item: routing it through the
/// webview for a confirm dialog only made the shortcut look broken when the
/// round-trip failed. ⌘W is rebound to our own "Close Tab" item so it emits to
/// the frontend (the client closes the active conversation, or ignores it)
/// instead of closing the only window. Cut/Copy/Paste/Select All are kept.
///
/// Undo/Redo are deliberately NOT added: the predefined items steal ⌘Z / ⌘⇧Z
/// before the event reaches the webview, which kills the app's own undo. Without
/// them ⌘Z falls through to JS, and focused text fields still get the webview's
/// built-in text undo.
fn build_app_menu(app: &tauri::App) -> tauri::Result<()> {
    use tauri::menu::{AboutMetadata, MenuBuilder, MenuItemBuilder, SubmenuBuilder};
    let handle = app.handle();
    let close_tab = MenuItemBuilder::new("Close Tab")
        .id("close_tab")
        .accelerator("CmdOrCtrl+W")
        .build(handle)?;
    let check_updates = MenuItemBuilder::new("Check for Updates…")
        .id("check_updates")
        .enabled(updater::updates_supported(handle))
        .build(handle)?;
    let app_menu = SubmenuBuilder::new(handle, "Gliph")
        .about(Some(AboutMetadata::default()))
        .separator()
        .item(&check_updates)
        .separator()
        .services()
        .separator()
        .hide()
        .hide_others()
        .show_all()
        .separator()
        .quit()
        .build()?;
    let edit_menu = SubmenuBuilder::new(handle, "Edit")
        .cut()
        .copy()
        .paste()
        .select_all()
        .build()?;
    let window_menu = SubmenuBuilder::new(handle, "Window")
        .minimize()
        .item(&close_tab)
        .build()?;
    let menu = MenuBuilder::new(handle)
        .items(&[&app_menu, &edit_menu, &window_menu])
        .build()?;
    handle.set_menu(menu)?;
    Ok(())
}

/// `tauri dev` runs the binary straight out of target/ with no .app around it.
/// With no Info.plist, macOS ignores `productName` and `bundle.icon` from the
/// config completely: the Dock shows a blank generic icon and the menu bar
/// shows the executable's filename. The only way to label a dev run is to tell
/// AppKit at runtime, which is what this does — the same trick inbox2 uses for
/// its dev Dock icon.
///
/// Bundled builds skip all of it: they already carry the right name and icon.
#[cfg(target_os = "macos")]
fn apply_dev_identity() {
    use objc2::rc::Retained;
    use objc2::runtime::AnyObject;
    use objc2::{msg_send, ClassType};
    use objc2_app_kit::{NSApplication, NSImage};
    use objc2_foundation::{MainThreadMarker, NSData};

    // Inside a bundle the path ends .../Gliph.app/Contents/MacOS/Gliph. If it
    // does not, this is a dev run.
    let bundled = std::env::current_exe()
        .map(|p| p.to_string_lossy().contains(".app/Contents/MacOS/"))
        .unwrap_or(false);
    if bundled {
        return;
    }

    let Some(mtm) = MainThreadMarker::new() else {
        return;
    };

    // The amber dev icon, embedded so a dev run needs no files on disk.
    const ICON_PNG: &[u8] = include_bytes!("../icons-dev/128x128@2x.png");
    let data = NSData::with_bytes(ICON_PNG);
    let image: Option<Retained<NSImage>> = unsafe {
        let alloc: *mut AnyObject = msg_send![NSImage::class(), alloc];
        let img: *mut AnyObject = msg_send![alloc, initWithData: &*data];
        Retained::from_raw(img.cast())
    };
    if let Some(image) = image {
        unsafe { NSApplication::sharedApplication(mtm).setApplicationIconImage(Some(&image)) };
    }

    log::info!("dev build: applied the dev Dock icon (unbundled run)");

    // Not attempted: renaming the app in the menu bar. For an unbundled binary
    // macOS takes that name from the executable's filename, not from any
    // CFBundleName we could set at runtime — writing into the main bundle's
    // infoDictionary changes nothing. The Dock icon is the dev label here; the
    // BUNDLED dev app (npm run install:app:dev) is a real .app and does say
    // "Gliph Dev" everywhere.
}

pub fn run() {
    install_panic_hook();

    let builder = tauri::Builder::default();

    // macOS: register tauri-nspanel so the notification window can be turned
    // into a non-activating panel — see notifypanel.rs for what that buys.
    #[cfg(target_os = "macos")]
    let builder = builder.plugin(tauri_nspanel::init());

    // Windows/Linux only, and registered BEFORE every other plugin so it can
    // hand argv to the live instance before the rest of the app starts booting
    // in this doomed second process.
    #[cfg(any(target_os = "windows", target_os = "linux"))]
    let builder = builder.plugin(tauri_plugin_single_instance::init(|app, _argv, _cwd| {
        if let Some(win) = app.get_webview_window("main") {
            // A minimized window ignores set_focus, so un-minimize first.
            if let Ok(true) = win.is_minimized() {
                let _ = win.unminimize();
            }
            let _ = win.show();
            let _ = win.set_focus();
        }
    }));

    let ctx = tauri::generate_context!();

    // Only when this build actually carries `plugins.updater` — which is to say,
    // only a release build (tauri.release.conf.json). The plugin refuses to
    // initialise without that config and takes the whole app down with it, so
    // registering it unconditionally meant every dev build and every locally
    // installed one panicked on launch. updates_supported() already tells the
    // menu to grey the item out where there is no updater.
    let builder = if ctx.config().plugins.0.contains_key("updater") {
        builder.plugin(tauri_plugin_updater::Builder::new().build())
    } else {
        builder
    };

    builder
        .plugin(tauri_plugin_opener::init())
        .plugin(tauri_plugin_notification::init())
        .plugin(
            tauri_plugin_log::Builder::default()
                .level(log::LevelFilter::Info)
                // The plugin's default max_file_size is 40 KB, which rotates the
                // interesting part away within minutes of a chatty session. Keep
                // ~10 MB so the reason we shut down is still in the file when we
                // go looking for it.
                .max_file_size(10_000_000)
                // The webview/windowing stack logs per-frame chatter at debug;
                // it drowns everything we actually wrote.
                .level_for("tao", log::LevelFilter::Warn)
                .level_for("wry", log::LevelFilter::Warn)
                .build(),
        )
        .plugin(
            // Drop MAXIMIZED from the tracked flags: with a transparent window
            // and an Overlay titlebar, is_maximized() is unreliable on macOS, so
            // the plugin persists a spurious maximized state and relaunches
            // maximized even when the last session was not.
            tauri_plugin_window_state::Builder::new()
                .with_state_flags(
                    tauri_plugin_window_state::StateFlags::all()
                        & !tauri_plugin_window_state::StateFlags::MAXIMIZED,
                )
                // The notification window's geometry is computed fresh on every
                // alert (top-right of whichever screen the app is on, fixed
                // width, height auto-fitted to the card stack). Persisting and
                // restoring it would clobber that with a stale size.
                .with_denylist(&["notify"])
                .build(),
        )
        .plugin(
            // A chat client renders links other people wrote. Anything that is
            // not our own bundle must open in the real browser, never inside the
            // app's webview.
            tauri::plugin::Builder::<tauri::Wry>::new("link-router")
                .on_navigation(|webview, url| {
                    let s = url.as_str();
                    let internal = s.starts_with("tauri://")
                        || s.starts_with("http://localhost")
                        || s.starts_with("http://127.0.0.1")
                        || s.starts_with("http://tauri.localhost")
                        || s.starts_with("https://tauri.localhost")
                        || s.starts_with("about:")
                        || s.starts_with("data:")
                        || s.starts_with("blob:");
                    if internal {
                        return true;
                    }
                    if s.starts_with("http://") || s.starts_with("https://") {
                        let _ = webview.emit("open-external-link", s.to_string());
                        return false; // cancel the in-frame navigation
                    }
                    true
                })
                .build(),
        )
        .setup(|app| {
            #[cfg(target_os = "macos")]
            apply_dev_identity();
            build_app_menu(app)?;
            log::info!("Starting Gliph desktop v{}", env!("CARGO_PKG_VERSION"));

            // The window-state plugin can reset the theme when it restores
            // geometry, so force it after setup rather than in the config.
            #[cfg(target_os = "macos")]
            if let Some(win) = app.get_webview_window("main") {
                let _ = win.set_theme(Some(tauri::Theme::Dark));
            }
            if updater::updates_supported(&app.handle().clone()) {
                updater::check_on_startup(&app.handle().clone());
            }
            let _ = app;
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            js_log,
            notifypanel::convert_notify_to_panel,
            devicelink::start_device_link,
            updater::check_for_updates,
            updater::install_update,
            mesh::mesh_start,
            mesh::mesh_stop,
            mesh::mesh_join,
            mesh::mesh_send,
            mesh::mesh_send_to,
            mesh::mesh_peers
        ])
        .on_menu_event(|app, event| {
            // ⌘W is our own item (see build_app_menu) precisely so it lands here
            // instead of the OS closing the only window.
            match event.id().as_ref() {
                "close_tab" => {
                    let _ = app.emit_to("main", "menu:close-tab", ());
                }
                "check_updates" => updater::check_now(app),
                _ => {}
            }
        })
        .on_window_event(|window, event| {
            // The red traffic light on the main window is a quit, not a close —
            // this is a single-window app.
            if let tauri::WindowEvent::CloseRequested { .. } = event {
                if window.label() == "main" {
                    window.app_handle().exit(0);
                }
            }
        })
        .build(ctx)
        .expect("error while building the Gliph application")
        .run(|app_handle, event| {
            if let tauri::RunEvent::ExitRequested { code, .. } = event {
                // macOS: hard-exit here instead of returning into wry's webview
                // teardown, which intermittently raises an Obj-C exception that
                // unwinds through tao and aborts, so the app appears to "crash on
                // quit". The window-state plugin only persists on RunEvent::Exit,
                // which we skip, so save it ourselves first. Never do this on a
                // restart: that path must reach RunEvent::Exit for Tauri to spawn
                // the replacement process.
                #[cfg(target_os = "macos")]
                if code != Some(tauri::RESTART_EXIT_CODE) {
                    use tauri_plugin_window_state::AppHandleExt;
                    let _ = app_handle.save_window_state(
                        tauri_plugin_window_state::StateFlags::all()
                            & !tauri_plugin_window_state::StateFlags::MAXIMIZED,
                    );
                    log::info!("Lifecycle: hard-exit (skipping wry teardown)");
                    std::process::exit(code.unwrap_or(0));
                }
                let _ = (app_handle, code);
            }
        });
}
