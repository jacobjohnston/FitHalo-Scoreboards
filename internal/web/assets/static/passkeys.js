/**
 * @fileoverview Runs a passkey ceremony for each button with data-passkey:
 * fetches the server's options, hands them to the browser, and posts the
 * answer back.
 */
'use strict';

(function() {
  const csrf = document.querySelector('meta[name="csrf"]');

  /**
   * Decodes unpadded base64url into bytes.
   * @param {string} value
   * @return {!Uint8Array}
   */
  function toBytes(value) {
    const base64 = value.replace(/-/g, '+').replace(/_/g, '/');
    const text = atob(base64 + '==='.slice((base64.length + 3) % 4));
    return Uint8Array.from(text, (character) => character.charCodeAt(0));
  }

  /**
   * Encodes bytes as unpadded base64url, or returns null for no bytes.
   * @param {?ArrayBuffer} buffer
   * @return {?string}
   */
  function toText(buffer) {
    if (!buffer) {
      return null;
    }
    const bytes = new Uint8Array(buffer);
    let text = '';
    bytes.forEach((byte) => {
      text += String.fromCharCode(byte);
    });
    return btoa(text)
        .replace(/\+/g, '-')
        .replace(/\//g, '_')
        .replace(/=+$/, '');
  }

  /**
   * Posts JSON with the page's CSRF token and returns the JSON answer.
   * @param {string} url
   * @param {!Object} body
   * @return {!Promise<!Object>}
   */
  async function post(url, body) {
    const headers = {'Content-Type': 'application/json'};
    if (csrf) {
      headers['X-CSRF-Token'] = csrf.content;
    }
    const response = await fetch(url, {
      method: 'POST',
      headers,
      body: JSON.stringify(body),
      credentials: 'same-origin',
    });
    if (!response.ok) {
      throw new Error('The server refused the passkey.');
    }
    return response.json();
  }

  /**
   * Creates a passkey and registers it under the name in the button's field.
   * @param {!HTMLButtonElement} button
   * @return {!Promise<!Object>}
   */
  async function register(button) {
    const name = document.getElementById(button.dataset.nameField);
    const options = await post(button.dataset.begin, {});
    const credential = await navigator.credentials.create({
      publicKey: {
        challenge: toBytes(options.challenge),
        rp: options.rp,
        user: {
          id: toBytes(options.user.id),
          name: options.user.name,
          displayName: options.user.displayName,
        },
        pubKeyCredParams: options.pubKeyCredParams,
        authenticatorSelection: options.authenticatorSelection,
        attestation: 'none',
        excludeCredentials: options.excludeCredentials.map(
            (item) => ({type: 'public-key', id: toBytes(item.id)})),
        timeout: options.timeout,
      },
    });
    return post(button.dataset.finish, {
      id: credential.id,
      clientDataJSON: toText(credential.response.clientDataJSON),
      attestationObject: toText(credential.response.attestationObject),
      name: name ? name.value : '',
    });
  }

  /**
   * Signs in with a discoverable passkey the browser offers.
   * @param {!HTMLButtonElement} button
   * @return {!Promise<!Object>}
   */
  async function signIn(button) {
    const options = await post(button.dataset.begin, {});
    const credential = await navigator.credentials.get({
      publicKey: {
        challenge: toBytes(options.challenge),
        rpId: options.rpId,
        userVerification: 'required',
        allowCredentials: [],
        timeout: options.timeout,
      },
    });
    return post(button.dataset.finish, {
      id: credential.id,
      clientDataJSON: toText(credential.response.clientDataJSON),
      authenticatorData: toText(credential.response.authenticatorData),
      signature: toText(credential.response.signature),
      userHandle: toText(credential.response.userHandle),
    });
  }

  document.querySelectorAll('[data-passkey]').forEach((button) => {
    const message = document.getElementById(button.dataset.message);
    if (!window.PublicKeyCredential) {
      button.hidden = true;
      if (message) {
        message.textContent = button.dataset.unsupported;
        message.hidden = false;
      }
      return;
    }
    button.addEventListener('click', async (event) => {
      event.preventDefault();
      button.disabled = true;
      try {
        const result = button.dataset.passkey === 'register' ?
            await register(button) :
            await signIn(button);
        window.location.assign(result.redirect);
      } catch (error) {
        if (message) {
          message.textContent = button.dataset.failed;
          message.hidden = false;
        }
        button.disabled = false;
      }
    });
  });
})();
