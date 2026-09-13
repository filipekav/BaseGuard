document.addEventListener('DOMContentLoaded', () => {
  const engine = document.querySelector('#engine');
  const port = document.querySelector('#port');
  engine?.addEventListener('change', () => {
    if (port.value === '5432' || port.value === '3306') port.value = engine.value === 'postgres' ? '5432' : '3306';
  });
  const schedule = document.querySelector('#schedule');
  const updateSchedule = () => document.querySelectorAll('[data-schedule]').forEach(label => {
    label.hidden = !label.dataset.schedule.split(' ').includes(schedule.value);
  });
  if (schedule) { schedule.addEventListener('change', updateSchedule); updateSchedule(); }
  document.querySelectorAll('[data-confirm]').forEach(form => form.addEventListener('submit', event => {
    if (!window.confirm(form.dataset.confirm)) event.preventDefault();
  }));
});
