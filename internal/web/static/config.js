// Configuration page: open the form dialog when a form arrives,
// close it when the server says "configSaved".
(function () {
  "use strict";
  const dlg = document.getElementById("form-modal");

  document.body.addEventListener("htmx:afterSwap", (e) => {
    if (e.detail.target.id !== "form-body") return;
    const form = e.detail.target.querySelector("form");
    if (form) document.getElementById("form-title").textContent = form.dataset.title;
    if (!dlg.open) dlg.showModal();
    const bad = e.detail.target.querySelector(".field-error");
    if (bad) bad.parentElement.querySelector("input,select")?.focus();
  });

  // Sent by the server (HX-Trigger header) after a successful save/delete.
  document.body.addEventListener("configSaved", (e) => {
    if (dlg.open) dlg.close();
    const text = e.detail && e.detail.value;
    if (text) {
      const msg = document.getElementById("config-msg");
      msg.innerHTML = "";
      const div = document.createElement("div");
      div.className = "msg ok";
      div.textContent = text;
      msg.appendChild(div);
    }
  });
})();

// PV form: show the analog or the digital fieldset depending on the
// register address typed (1xxxx = digital input).
(function () {
  "use strict";
  function sync(form) {
    const reg = Number(form.querySelector("[name=register_address]").value);
    const digital = reg >= 10001 && reg <= 19999;
    form.querySelectorAll(".analog-only").forEach((el) => { el.hidden = digital; });
    form.querySelectorAll(".digital-only").forEach((el) => { el.hidden = !digital; });
    // Hidden analog inputs must not block submit with "required".
    form.querySelectorAll(".analog-only [name=pv_min], .analog-only [name=pv_max]")
        .forEach((el) => { el.required = !digital; });
  }
  document.body.addEventListener("htmx:afterSwap", (e) => {
    const form = e.detail.target.querySelector("#pv-form");
    if (!form) return;
    sync(form);
    form.querySelector("[name=register_address]").addEventListener("input", () => sync(form));
  });
})();
