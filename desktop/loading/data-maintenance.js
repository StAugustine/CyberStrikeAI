const invoke = window.__TAURI__.core.invoke;
const message = document.querySelector('#maintenance-message');
const pendingPanel = document.querySelector('#pending-import');
const noPending = document.querySelector('#no-pending-import');
const backupList = document.querySelector('#backup-list');
let pendingImport = null;
let busy = false;

function setMessage(text, state = '') {
  message.textContent = text || '';
  message.dataset.state = state;
}

function setBusy(value) {
  busy = value;
  document.querySelectorAll('button').forEach((button) => {
    button.disabled = value;
  });
  if (!value && pendingImport) {
    document.querySelectorAll('[data-maintenance-action="restore"]').forEach((button) => {
      button.disabled = true;
    });
  }
}

function formatBytes(value) {
  const bytes = Number(value) || 0;
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  return `${(bytes / (1024 * 1024 * 1024)).toFixed(1)} GB`;
}

function formatTime(value) {
  if (!value) return '-';
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? '-' : parsed.toLocaleString();
}

function renderItems(element, values, emptyText) {
  element.replaceChildren();
  const items = Array.isArray(values) && values.length ? values : [emptyText];
  items.forEach((value) => {
    const item = document.createElement('li');
    item.textContent = String(value);
    element.append(item);
  });
}

function renderImport(state) {
  pendingImport = state || null;
  pendingPanel.hidden = !pendingImport;
  noPending.hidden = Boolean(pendingImport);
  if (!pendingImport) return;

  const report = pendingImport.report || {};
  document.querySelector('#import-source-version').textContent = report.source_version || '-';
  document.querySelector('#import-target-version').textContent = report.target_version || '-';
  document.querySelector('#import-file-count').textContent = String(report.file_count || 0);
  document.querySelector('#import-total-bytes').textContent = formatBytes(report.total_bytes);
  renderItems(document.querySelector('#import-sections'), report.imported_sections, '没有可导入内容');
  renderItems(
    document.querySelector('#import-credentials'),
    [...(report.plaintext_credential_paths || []), ...(report.reference_credential_paths || [])],
    '没有检测到凭据字段'
  );
  renderItems(document.querySelector('#import-excluded'), report.excluded_capabilities, '没有额外排除项');
  renderItems(document.querySelector('#import-warnings'), report.warnings, '预检未发现额外风险');
  const removed = Number(report.removed_user_accounts) || 0;
  document.querySelector('#removed-users').textContent = removed
    ? `已从待导入副本移除 ${removed} 个非管理员账号及其自定义权限；当前数据尚未更改。`
    : '当前数据尚未更改；确认后会先创建回滚点。';
}

function createMeta(label, value) {
  const item = document.createElement('span');
  const name = document.createElement('strong');
  name.textContent = `${label}：`;
  item.append(name, document.createTextNode(value));
  return item;
}

function renderBackups(backups) {
  backupList.replaceChildren();
  if (!Array.isArray(backups) || backups.length === 0) {
    const empty = document.createElement('div');
    empty.className = 'empty-state';
    empty.textContent = '暂无恢复点。完成升级、导入或恢复前操作后会自动创建。';
    backupList.append(empty);
    return;
  }

  backups.forEach((backup) => {
    const card = document.createElement('article');
    card.className = 'backup-card';
    const heading = document.createElement('div');
    heading.className = 'backup-heading';
    const title = document.createElement('div');
    const name = document.createElement('h3');
    name.textContent = backup.id || '未知恢复点';
    const badge = document.createElement('span');
    badge.className = backup.valid ? 'status-badge valid' : 'status-badge invalid';
    badge.textContent = backup.valid ? (backup.protected ? '事务保护' : backup.retained ? '保留' : '可清理') : '校验失败';
    title.append(name, badge);
    heading.append(title);

    const actions = document.createElement('div');
    actions.className = 'backup-actions';
    if (backup.valid) {
      const restore = document.createElement('button');
      restore.type = 'button';
      restore.className = 'secondary compact';
      restore.dataset.maintenanceAction = 'restore';
      restore.textContent = '恢复';
      restore.disabled = Boolean(pendingImport) || busy;
      restore.addEventListener('click', () => restoreBackup(backup.id));
      actions.append(restore);
    }
    if (backup.deletable) {
      const remove = document.createElement('button');
      remove.type = 'button';
      remove.className = 'danger compact';
      remove.textContent = '删除';
      remove.disabled = busy;
      remove.addEventListener('click', () => deleteBackup(backup.id));
      actions.append(remove);
    }
    heading.append(actions);
    card.append(heading);

    const meta = document.createElement('div');
    meta.className = 'backup-meta';
    meta.append(
      createMeta('类型', backup.kind || '-'),
      createMeta('版本', `${backup.from_version || '-'} → ${backup.to_version || '-'}`),
      createMeta('创建', formatTime(backup.created_at)),
      createMeta('内容', `${backup.file_count || 0} 个文件 / ${formatBytes(backup.total_bytes)}`)
    );
    card.append(meta);
    if (!backup.valid) {
      const warning = document.createElement('p');
      warning.className = 'backup-warning';
      warning.textContent = '该目录未通过完整性校验，不允许从界面恢复或删除。请保留现场并查看本地日志。';
      card.append(warning);
    }
    backupList.append(card);
  });
}

async function loadState({ preserveMessage = false } = {}) {
  if (!preserveMessage) setMessage('正在读取本地恢复点…', 'pending');
  const state = await invoke('get_data_maintenance_state');
  renderImport(state.pending_import || null);
  renderBackups(state.backups || []);
  if (!preserveMessage) setMessage('数据维护状态已更新。', 'success');
}

async function waitUntilReady() {
  for (let attempt = 0; attempt < 40; attempt += 1) {
    try {
      await loadState({ preserveMessage: true });
      return true;
    } catch (_) {
      await new Promise((resolve) => setTimeout(resolve, 250));
    }
  }
  return false;
}

async function runLiveOperation(operation, pendingText, successText) {
  if (busy) return;
  setBusy(true);
  setMessage(pendingText, 'pending');
  try {
    const result = await operation();
    await loadState({ preserveMessage: true });
    setMessage(typeof successText === 'function' ? successText(result) : successText, 'success');
  } catch (error) {
    setMessage(typeof error === 'string' ? error : '数据维护操作失败。', 'error');
  } finally {
    setBusy(false);
  }
}

async function runOfflineOperation(command, parameters, pendingText) {
  if (busy) return;
  setBusy(true);
  setMessage(pendingText, 'pending');
  try {
    await invoke(command, parameters);
    setMessage('操作已完成，正在安全重启本地核心…', 'success');
  } catch (error) {
    const safeError = typeof error === 'string' ? error : '数据维护操作失败。';
    setMessage(`${safeError} 正在恢复客户端连接…`, 'error');
    await waitUntilReady();
    setMessage(safeError, 'error');
    setBusy(false);
  }
}

function restoreBackup(backupId) {
  if (pendingImport) {
    setMessage('请先确认或取消待导入数据，再恢复历史恢复点。', 'error');
    return;
  }
  if (!window.confirm(`将当前数据回滚到恢复点“${backupId}”。继续前会自动创建当前状态的恢复点，是否继续？`)) return;
  runOfflineOperation('restore_desktop_backup', { backupId }, '正在停止本地核心并创建回滚点，请勿关闭客户端…');
}

function deleteBackup(backupId) {
  if (!window.confirm(`永久删除恢复点“${backupId}”？保留策略保护的恢复点不会被删除。`)) return;
  runLiveOperation(
    () => invoke('delete_desktop_backup', { backupId }),
    '正在校验并删除恢复点…',
    '恢复点已删除。'
  );
}

document.querySelector('#select-import').addEventListener('click', () => {
  runLiveOperation(
    () => invoke('choose_and_prepare_legacy_import'),
    '正在校验旧版数据并创建隔离副本…',
    (result) => result ? '旧版数据预检完成，请审查后确认。' : '已取消目录选择，当前数据未改变。'
  );
});

document.querySelector('#confirm-import').addEventListener('click', () => {
  if (!pendingImport) return;
  if (!window.confirm('确认导入预检结果？当前状态会先保存为可恢复的回滚点，然后重启本地核心。')) return;
  runOfflineOperation('confirm_legacy_import', {}, '正在停止本地核心、创建回滚点并导入数据，请勿关闭客户端…');
});

document.querySelector('#cancel-import').addEventListener('click', () => {
  if (!pendingImport || !window.confirm('取消并删除这份待导入副本？当前客户端数据不会改变。')) return;
  runLiveOperation(
    () => invoke('cancel_legacy_import'),
    '正在取消待导入数据…',
    '待导入副本已取消，当前数据未改变。'
  );
});

document.querySelector('#refresh-maintenance').addEventListener('click', () => {
  runLiveOperation(() => Promise.resolve(), '正在刷新…', '数据维护状态已更新。');
});

document.querySelector('#close-maintenance').addEventListener('click', async () => {
  if (busy) return;
  setBusy(true);
  try {
    await invoke('close_data_maintenance');
  } catch (error) {
    setMessage(typeof error === 'string' ? error : '暂时无法返回客户端。', 'error');
    setBusy(false);
  }
});

setBusy(true);
loadState()
  .catch((error) => {
    setMessage(typeof error === 'string' ? error : '无法读取数据维护状态。', 'error');
  })
  .finally(() => setBusy(false));
