const form = document.querySelector('#bootstrap-form');
const passwordInput = document.querySelector('#password');
const confirmationInput = document.querySelector('#confirmation');
const submitButton = document.querySelector('#submit');
const message = document.querySelector('#message');

form.addEventListener('submit', async (event) => {
  event.preventDefault();
  message.textContent = '';

  const password = passwordInput.value;
  if (password.length < 8) {
    message.textContent = 'Use at least 8 characters.';
    passwordInput.focus();
    return;
  }
  if (password !== confirmationInput.value) {
    message.textContent = 'The passwords do not match.';
    confirmationInput.focus();
    return;
  }

  submitButton.disabled = true;
  try {
    await window.__TAURI__.core.invoke('submit_bootstrap_password', { password });
    passwordInput.value = '';
    confirmationInput.value = '';
    message.textContent = 'Initializing your secure workspace…';
  } catch (error) {
    passwordInput.value = '';
    confirmationInput.value = '';
    message.textContent = typeof error === 'string' ? error : 'Initialization failed. Try again.';
    submitButton.disabled = false;
    passwordInput.focus();
  }
});
