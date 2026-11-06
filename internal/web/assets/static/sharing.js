/**
 * @fileoverview Keeps the Rules page's sharing options consistent: activities
 * only with score details, routes and strength sets only with activities, and
 * weights only with strength sets. The server applies the same rules.
 */
'use strict';

(function() {
  const group = document.querySelector('[data-sharing-options]');
  if (!group) {
    return;
  }
  const score = group.querySelector('[data-sharing-score]');
  const activity = group.querySelector('[data-sharing-activity]');
  const options = group.querySelectorAll('[data-sharing-option]');
  const sets = group.querySelector('[data-sharing-sets]');
  const weights = group.querySelector('[data-sharing-weights]');

  /**
   * Enables the checkbox, or disables and clears it.
   * @param {!HTMLInputElement} input
   * @param {boolean} allowed
   */
  function allow(input, allowed) {
    if (!allowed) {
      input.checked = false;
    }
    input.disabled = !allowed;
  }

  /** Disables and clears each option its prerequisite no longer allows. */
  function update() {
    const hasDetails = score.value === 'details';
    if (!hasDetails) {
      activity.value = 'none';
    }
    activity.disabled = !hasDetails;
    const hasActivities = hasDetails && activity.value !== 'none';
    options.forEach((input) => allow(input, hasActivities));
    allow(weights, hasActivities && sets.checked);
  }

  [score, activity, sets].forEach(
      (input) => input.addEventListener('change', update));
  update();
})();
