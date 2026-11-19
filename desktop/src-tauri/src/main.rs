// Release builds on Windows must not spawn an extra console window.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

fn main() {
    gliph_desktop_lib::run()
}
