/**
 * @fileoverview Flashes the warning a data-warn-on-change select names when
 * the select leaves its saved value, for settings that cannot be undone.
 */
'use strict';

(function() {
  document.querySelectorAll('[data-warn-on-change]').forEach((select) => {
    const warning = document.getElementById(select.dataset.warnOnChange);
    const saved = select.value;
    if (!warning) {
      return;
    }
    select.addEventListener('change', () => {
      warning.classList.remove('is-changed');
      if (select.value !== saved) {
        // Reading the layout between the two restarts the flash every time.
        void warning.offsetWidth;
        warning.classList.add('is-changed');
      }
    });
  });
})();
