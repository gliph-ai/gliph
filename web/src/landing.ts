import "./landing.css";
import { markSignedIn, session } from "./session";

// A reader signed in before the cookie hint existed has a session and no
// cookie: set it and go to the app.
if (session().token) {
  markSignedIn(true);
  location.replace("/");
}

for (const b of document.querySelectorAll<HTMLButtonElement>("button[data-copy]")) {
  b.addEventListener("click", async () => {
    const text = document.getElementById(b.dataset.copy!)?.textContent ?? "";
    try {
      await navigator.clipboard.writeText(text);
      b.textContent = "Copied";
    } catch {
      b.textContent = "Select it";
    }
    setTimeout(() => { b.textContent = "Copy"; }, 1600);
  });
}
