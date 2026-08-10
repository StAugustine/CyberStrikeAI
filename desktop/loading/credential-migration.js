const pathsList = document.querySelector('#credential-paths');
const confirmButton = document.querySelector('#confirm');
const cancelButton = document.querySelector('#cancel');
const message = document.querySelector('#message');

async function loadPaths() {
  try {
    const paths = await window.__TAURI__.core.invoke('get_credential_migration_paths');
    for (const path of paths) {
      const item = document.createElement('li');
      item.textContent = path;
      pathsList.append(item);
    }
  } catch (error) {
    message.textContent = typeof error === 'string' ? error : 'Unable to load migration details.';
    confirmButton.disabled = true;
  }
}

confirmButton.addEventListener('click', async () => {
  confirmButton.disabled = true;
  cancelButton.disabled = true;
  message.textContent = 'Protecting credentials…';
  try {
    await window.__TAURI__.core.invoke('confirm_credential_migration');
  } catch (error) {
    message.textContent = typeof error === 'string' ? error : 'Credential migration failed.';
    confirmButton.disabled = false;
    cancelButton.disabled = false;
  }
});

cancelButton.addEventListener('click', async () => {
  confirmButton.disabled = true;
  cancelButton.disabled = true;
  message.textContent = 'Exiting without changing credentials…';
  try {
    await window.__TAURI__.core.invoke('cancel_credential_migration');
  } catch (error) {
    message.textContent = typeof error === 'string' ? error : 'Unable to exit safely.';
    confirmButton.disabled = false;
    cancelButton.disabled = false;
  }
});

loadPaths();
