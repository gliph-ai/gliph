//! macOS: turn the notification window into a *non-activating* NSPanel.
//!
//! The notification window (web/notify.html, created by web/src/notify.ts) is a
//! separate window owned by this app. On macOS, clicking anything inside it
//! would normally activate Gliph and yank the main window in front of whatever
//! the user was actually typing in — which is precisely the failure that makes
//! a bespoke notification window worse than handing the alert to the OS. A
//! non-activating panel takes clicks without activating its owner, so
//! dismissing a card leaves the user's app exactly where it was. Clicking the
//! card body still raises Gliph, on purpose, via the `notify-open` event.
//!
//! Idempotent, and it has to be: the window is persistent (hidden between
//! alerts, never destroyed) and converted exactly once. tauri-nspanel does not
//! clean its panel registry up when a window is destroyed, so converting a
//! recreated window of the same label would leave a dangling `RawNSPanel` and
//! double-free on quit. Hence: create once, convert once, hide instead of close.
//!
//! On Windows and Linux this is a no-op — the window's `focus: false` +
//! `skipTaskbar` + `alwaysOnTop` flags already give equivalent behaviour there.

/// Convert the `notify` window to a non-activating floating NSPanel.
#[tauri::command]
pub fn convert_notify_to_panel(app: tauri::AppHandle) -> Result<(), String> {
    #[cfg(target_os = "macos")]
    {
        use tauri::Manager;
        let Some(window) = app.get_webview_window("notify") else {
            return Err("notification window not found".into());
        };
        let app = app.clone();
        // The class swap and the msg_sends below are AppKit mutation: main
        // thread only.
        window
            .run_on_main_thread(move || {
                use tauri_nspanel::{ManagerExt, WebviewWindowExt};
                if app.get_webview_panel("notify").is_ok() {
                    return; // already converted — do not re-insert into the store
                }
                let Some(win) = app.get_webview_window("notify") else {
                    return;
                };
                match win.to_panel() {
                    Ok(panel) => {
                        // 1 << 7 = NSWindowStyleMaskNonactivatingPanel.
                        panel.set_style_mask(1 << 7);
                        panel.set_becomes_key_only_if_needed(true);
                        panel.set_floating_panel(true);
                        // NSFloatingWindowLevel — above ordinary windows, which
                        // is what the plain window's always-on-top was doing.
                        panel.set_level(3);
                        // We hide it between alerts and only tear it down on
                        // quit; let Tauri own the lifetime so a close() never
                        // releases a window the plugin still holds.
                        panel.set_released_when_closed(false);
                        install_focus_overrides();
                        log::info!("notify window: converted to a non-activating NSPanel");
                    }
                    Err(_) => log::warn!("notify window: to_panel conversion failed"),
                }
            })
            .map_err(|e| e.to_string())?;
    }
    #[cfg(not(target_os = "macos"))]
    let _ = app;
    Ok(())
}

/// Keep the panel from ever taking keyboard focus, while still letting a single
/// click reach the card. Installed once, on the main thread, after `to_panel()`
/// has registered the `RawNSPanel` class.
///
/// 1. `-[RawNSPanel canBecomeKeyWindow]` → NO. tauri-nspanel hardcodes YES, so
///    `becomesKeyOnlyIfNeeded` is not enough: a click landing in the WKWebView
///    still promotes the panel to key window and pulls keyboard focus off the
///    app the user is typing in. Overriding it at the window level (rather than
///    per hit-view, which the webview's private subviews slip past) means the
///    panel can never become key. There is no text input on a card, so it never
///    needs to be.
/// 2. `-[NSView acceptsFirstMouse:]` → YES for views inside an `NSPanel`. With
///    the panel never key, every click is a "first mouse" on an inactive window,
///    which AppKit otherwise swallows as "make this window key" — so the ×
///    button would need two clicks. Gated to panel views, so every other window
///    chains through to the original and keeps stock behaviour.
#[cfg(target_os = "macos")]
fn install_focus_overrides() {
    use objc::runtime::{
        class_getInstanceMethod, method_getImplementation, method_setImplementation, Class, Imp,
        Method, Object, Sel, BOOL, NO, YES,
    };
    use objc::{class, msg_send, sel, sel_impl};
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::sync::Once;

    // The original IMP, so non-panel views keep stock behaviour. Held as usize
    // because function pointers are not atomic.
    static ORIG_FIRST_MOUSE: AtomicUsize = AtomicUsize::new(0);
    static SWIZZLE: Once = Once::new();

    extern "C" fn cannot_become_key(_this: &Object, _cmd: Sel) -> BOOL {
        NO
    }

    extern "C" fn accepts_first_mouse(this: &Object, cmd: Sel, event: *mut Object) -> BOOL {
        unsafe {
            let window: *mut Object = msg_send![this, window];
            if !window.is_null() {
                let is_panel: BOOL = msg_send![window, isKindOfClass: class!(NSPanel)];
                if is_panel != NO {
                    return YES;
                }
            }
            let original = ORIG_FIRST_MOUSE.load(Ordering::SeqCst);
            if original != 0 {
                let original: extern "C" fn(&Object, Sel, *mut Object) -> BOOL =
                    std::mem::transmute(original);
                return original(this, cmd, event);
            }
            NO
        }
    }

    SWIZZLE.call_once(|| unsafe {
        match Class::get("RawNSPanel") {
            Some(cls) => {
                let m = class_getInstanceMethod(cls, sel!(canBecomeKeyWindow));
                if m.is_null() {
                    log::warn!("canBecomeKeyWindow not found on RawNSPanel — skipping");
                } else {
                    let imp: Imp = std::mem::transmute(
                        cannot_become_key as extern "C" fn(&Object, Sel) -> BOOL,
                    );
                    method_setImplementation(m as *mut Method, imp);
                }
            }
            None => log::warn!("RawNSPanel class not registered — skipping focus overrides"),
        }

        let view = class!(NSView);
        let m = class_getInstanceMethod(view, sel!(acceptsFirstMouse:));
        if m.is_null() {
            log::warn!("acceptsFirstMouse: not found on NSView — skipping");
        } else {
            ORIG_FIRST_MOUSE.store(
                method_getImplementation(m as *mut Method) as usize,
                Ordering::SeqCst,
            );
            let imp: Imp = std::mem::transmute(
                accepts_first_mouse as extern "C" fn(&Object, Sel, *mut Object) -> BOOL,
            );
            method_setImplementation(m as *mut Method, imp);
        }
    });
}
