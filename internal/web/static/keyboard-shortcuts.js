// ========== 快捷键系统 ==========
const findSearch = () => document.querySelector('#history-filter, #catalog-query, #chain-query, #evidence-query');
document.addEventListener('keydown', e => {
  if ((e.ctrlKey || e.metaKey) && e.key === 'k') {
    const searchBox = findSearch();
    if (searchBox) { e.preventDefault(); searchBox.focus(); searchBox.select(); }
  }
  if (e.key === 'Escape') {
    const dialog = document.getElementById('detail-dialog');
    if (dialog && dialog.open) { dialog.close(); return; }
    if (document.activeElement.tagName === 'INPUT' && document.activeElement.type === 'search') {
      document.activeElement.blur();
    }
  }
  if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
    const textarea = document.querySelector('.composer textarea:focus');
    if (textarea) {
      e.preventDefault();
      const form = textarea.closest('form');
      if (form) form.requestSubmit();
    }
  }
  if (e.key === '/' && !document.activeElement.isContentEditable && !['INPUT', 'TEXTAREA', 'SELECT'].includes(document.activeElement.tagName)) {
    const searchBox = findSearch();
    if (searchBox) { e.preventDefault(); searchBox.focus(); }
  }
});
