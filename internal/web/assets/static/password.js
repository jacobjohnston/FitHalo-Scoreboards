/**
 * @fileoverview Checks a new admin password as it is typed: its length as a
 * person counts characters, a strength estimate, whether the two fields match,
 * and whether Have I Been Pwned lists it. Only the first five characters of its
 * SHA-1 leave the page, and the server checks length and breach again.
 */
'use strict';

(function() {
  const breachRangeURL = 'https://api.pwnedpasswords.com/range/';
  const breachDelay = 400;
  const excellentEntropyBits = 100;
  const levels = [
    [40, 'weak'],
    [55, 'moderate'],
    [75, 'good'],
    [Infinity, 'excellent'],
  ];
  const minimumScore = 10;
  const sequences = [
    '0123456789', '9876543210',
    'abcdefghijklmnopqrstuvwxyz', 'zyxwvutsrqponmlkjihgfedcba',
    'qwertyuiop', 'poiuytrewq', 'asdfghjkl', 'lkjhgfdsa', 'zxcvbnm', 'mnbvcxz',
  ];
  const segmenter = typeof Intl !== 'undefined' && Intl.Segmenter ?
      new Intl.Segmenter() :
      null;

  /**
   * Splits text into the characters a person sees (grapheme clusters).
   * @param {string} text
   * @return {!Array<string>}
   */
  function characters(text) {
    return segmenter ?
        Array.from(segmenter.segment(text), (part) => part.segment) :
        Array.from(text);
  }

  /**
   * Shortens each run of four or more from a common sequence to its first
   * character, as the rest is predictable.
   * @param {string} text
   * @return {string}
   */
  function collapseSequences(text) {
    let result = text;
    for (const sequence of sequences) {
      for (let start = 0; start < sequence.length - 3; start++) {
        for (let end = sequence.length; end >= start + 4; end--) {
          const run = sequence.substring(start, end);
          const found = result.toLowerCase().indexOf(run);
          if (found >= 0) {
            result = result.slice(0, found + 1) +
                result.slice(found + run.length);
            start = end - 1;
            break;
          }
        }
      }
    }
    return result;
  }

  /**
   * Estimates entropy in bits: the larger of the character-set and the
   * distinct-character estimates.
   * @param {string} text
   * @return {number}
   */
  function entropy(text) {
    const parts = characters(text);
    let setSize = 0;
    setSize += /[a-z]/.test(text) ? 26 : 0;
    setSize += /[A-Z]/.test(text) ? 26 : 0;
    setSize += /\d/.test(text) ? 10 : 0;
    setSize += /[^a-zA-Z0-9]/.test(text) ? 32 : 0;
    const fromSets = setSize > 0 ? parts.length * Math.log2(setSize) : 0;
    const unique = new Set(parts).size;
    const fromUnique = unique > 0 ? parts.length * Math.log2(unique) : 0;
    return Math.max(fromSets, fromUnique);
  }

  /**
   * Returns the strength level's name and the bar's width as a percentage.
   * @param {string} text
   * @param {number} length The length in characters a person sees.
   * @param {number} minimum The shortest password the server accepts.
   * @return {!Array<string|number>}
   */
  function strengthLevel(text, length, minimum) {
    if (length === 0) {
      return ['none', 0];
    }
    if (length < minimum) {
      return ['short', 0];
    }
    const bits = entropy(collapseSequences(text));
    const score = Math.max(
        minimumScore, Math.min(100, bits / excellentEntropyBits * 100));
    const level = levels.find(([ceiling]) => score <= ceiling)[1];
    return [level, score];
  }

  /**
   * Returns the text's SHA-1 in uppercase hexadecimal.
   * @param {string} text
   * @return {!Promise<string>}
   */
  async function sha1(text) {
    const digest = await crypto.subtle.digest(
        'SHA-1', new TextEncoder().encode(text));
    const bytes = Array.from(
        new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, '0'));
    return bytes.join('').toUpperCase();
  }

  /**
   * Asks the breach list's range API whether it holds the text, by k-anonymity.
   * @param {string} text
   * @return {!Promise<boolean>}
   */
  async function isBreached(text) {
    const hash = await sha1(text);
    const response = await fetch(
        breachRangeURL + hash.slice(0, 5), {headers: {'Add-Padding': 'true'}});
    if (!response.ok) {
      throw new Error('the breach check answered ' + response.status);
    }
    const suffix = hash.slice(5);
    return (await response.text()).split('\n').some((line) => {
      const [candidate, count] = line.trim().split(':');
      return candidate === suffix && count !== '0';
    });
  }

  /**
   * Wires the checks to one new-password block and its form's submit button.
   * @param {!HTMLElement} container The element with data-password-check.
   */
  function attach(container) {
    const form = container.closest('form');
    const submit = form ? form.querySelector('button[type="submit"]') : null;
    const passwordField = container.querySelector('[data-new-password]');
    const confirmField = container.querySelector('[data-confirm-password]');
    const strength = container.querySelector('[data-strength]');
    const bar = container.querySelector('[data-strength-bar]');

    /**
     * Returns the rule's list item.
     * @param {string} name
     * @return {?HTMLElement}
     */
    const rule = (name) =>
        container.querySelector('[data-rule="' + name + '"]');

    const minimum = Number(container.dataset.minimum);
    const maximum = Number(container.dataset.maximum);
    let breachState = 'pending';
    let checkedState = null;
    let checkedText = null;
    let request = 0;
    let timer = null;

    /** Shows each check's state and enables submit once they allow it. */
    function update() {
      const text = passwordField.value;
      const length = characters(text).length;
      const [level, score] = strengthLevel(text, length, minimum);
      strength.dataset.level = level;
      bar.style.width = score + '%';

      const lengthState =
          length < minimum ? 'fail' : length > maximum ? 'long' : 'pass';
      rule('length').dataset.state = lengthState;
      const matches = text !== '' && text === confirmField.value;
      rule('match').dataset.state = matches ? 'pass' : 'fail';

      if (lengthState !== 'pass') {
        breachState = 'pending';
      } else {
        breachState = text === checkedText ? checkedState : 'checking';
      }
      rule('breach').dataset.state = breachState;

      // A breach list that cannot be reached never blocks the form.
      const breachSettled = breachState === 'pass' || breachState === 'error';
      if (submit) {
        submit.disabled = !(lengthState === 'pass' && matches && breachSettled);
      }
    }

    /** Checks the password against the breach list once typing pauses. */
    function scheduleBreachCheck() {
      clearTimeout(timer);
      const text = passwordField.value;
      const length = characters(text).length;
      if (length < minimum || length > maximum || text === checkedText) {
        return;
      }
      const thisRequest = ++request;
      timer = setTimeout(async () => {
        let state;
        try {
          state = (await isBreached(text)) ? 'fail' : 'pass';
        } catch (error) {
          state = 'error';
        }
        if (thisRequest !== request) {
          return;
        }
        checkedText = text;
        checkedState = state;
        update();
      }, breachDelay);
    }

    passwordField.addEventListener('input', () => {
      update();
      scheduleBreachCheck();
    });
    confirmField.addEventListener('input', update);
    update();
  }

  document.querySelectorAll('[data-password-check]').forEach(attach);
})();
