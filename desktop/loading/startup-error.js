const title = document.querySelector('#failure-title');
const description = document.querySelector('#failure-message');
const code = document.querySelector('#failure-code');
const message = document.querySelector('#message');
const retryButton = document.querySelector('#retry');
const exitButton = document.querySelector('#exit');
const buttons = Array.from(document.querySelectorAll('button'));

async function loadFailure() {
  try {
    const failure = await window.__TAURI__.core.invoke('get_startup_failure');
    title.textContent = failure.title;
    description.textContent = failure.message;
    code.textContent = failure.code;
  } catch (_) {
    message.textContent = 'Startup diagnostics are unavailable. Open the logs for details.';
  }
}

async function openDirectory(directory) {
  message.textContent = '';
  try {
    await window.__TAURI__.core.invoke('open_desktop_directory', { directory });
  } catch (error) {
    message.textContent = typeof error === 'string' ? error : 'The directory could not be opened.';
  }
}

retryButton.addEventListener('click', async () => {
  buttons.forEach((button) => { button.disabled = true; });
  message.textContent = 'Retrying secure local startup…';
  try {
    await window.__TAURI__.core.invoke('retry_startup');
  } catch (error) {
    message.textContent = typeof error === 'string' ? error : 'Startup retry failed.';
    buttons.forEach((button) => { button.disabled = false; });
    await loadFailure();
  }
});

document.querySelector('#open-logs').addEventListener('click', () => openDirectory('logs'));
document.querySelector('#open-data').addEventListener('click', () => openDirectory('data'));
exitButton.addEventListener('click', async () => {
  buttons.forEach((button) => { button.disabled = true; });
  await window.__TAURI__.core.invoke('exit_after_startup_failure');
});

loadFailure();
