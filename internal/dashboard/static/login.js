// Sign-in page of the fleetreeve console.
"use strict";
(function () {
  try {
    const t = localStorage.getItem("fleetreeve-theme") || (matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
    document.documentElement.dataset.theme = t;
  } catch (_) { /* private mode */ }

  const form = document.getElementById("form"), error = document.getElementById("error"), submit = document.getElementById("submit");
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    error.hidden = true;
    const user = document.getElementById("user").value.trim(), password = document.getElementById("password").value;
    if (!user || !password) {
      error.textContent = "Enter your name and password.";
      error.hidden = false;
      return;
    }
    submit.disabled = true;
    submit.textContent = "Signing in…";
    try {
      const r = await fetch("api/login", { method: "POST", headers: { "Content-Type": "application/json", "X-Fleetreeve-Console": "1" }, body: JSON.stringify({ user, password }) });
      if (r.ok) { location.replace("./"); return; }
      error.textContent = (await r.json().catch(() => ({}))).error || "Signing in did not work. Try again.";
    } catch (_) {
      error.textContent = "The console cannot be reached. Check your connection and try again.";
    }
    error.hidden = false;
    submit.disabled = false;
    submit.textContent = "Sign in";
    document.getElementById("password").select();
  });
})();
