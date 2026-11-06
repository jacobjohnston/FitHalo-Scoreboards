/**
 * @fileoverview Scales the image chosen for the board's avatar down to the size
 * the board stores, so a phone photo uploads within the board's limits, and
 * previews it in place of the current avatar.
 */
'use strict';

(function() {
  /** The longest side the board stores, matching avatar.MaximumSide. */
  const MAXIMUM_SIDE = 512;

  /** The JPEG quality of the scaled image; the board re-encodes it anyway. */
  const QUALITY = 0.92;

  const input = document.querySelector('[data-avatar-input]');
  const preview = document.querySelector('[data-avatar-preview]');
  if (!input || !preview || !window.createImageBitmap || !window.DataTransfer) {
    return;
  }
  input.addEventListener('change', async () => {
    const file = input.files[0];
    if (!file) {
      return;
    }
    let bitmap;
    try {
      bitmap = await createImageBitmap(file);
    } catch (error) {
      // The board explains an image the browser cannot read when it is saved.
      return;
    }
    const scale = Math.min(1, MAXIMUM_SIDE / Math.max(bitmap.width, bitmap.height));
    const canvas = document.createElement('canvas');
    canvas.width = Math.max(1, Math.round(bitmap.width * scale));
    canvas.height = Math.max(1, Math.round(bitmap.height * scale));
    const context = canvas.getContext('2d');
    // JPEG has no transparency, so transparent pixels become white, as on the board.
    context.fillStyle = '#ffffff';
    context.fillRect(0, 0, canvas.width, canvas.height);
    context.drawImage(bitmap, 0, 0, canvas.width, canvas.height);
    bitmap.close();
    preview.replaceChildren(canvas);
    const removal = input.form.querySelector('[name="removeAvatar"]');
    if (removal) {
      removal.checked = false;
    }
    canvas.toBlob((blob) => {
      if (!blob) {
        return;
      }
      const transfer = new DataTransfer();
      transfer.items.add(new File([blob], 'avatar.jpg', {type: 'image/jpeg'}));
      input.files = transfer.files;
    }, 'image/jpeg', QUALITY);
  });
})();
