/** @type {import('tailwindcss').Config} */
module.exports = {
  content: ['./views/*.templ'],
  theme: { extend: {
    fontFamily: { sans: ['Instrument Sans', 'ui-sans-serif', 'system-ui', 'sans-serif'], display: ['Bricolage Grotesque', 'ui-sans-serif', 'sans-serif'] },
    colors: { ink: '#15121A', panel: '#1F1A26', line: '#2F2838', fg: '#EDE7F2', muted: '#968CA3', amber: { DEFAULT: '#F4B740', hi: '#FFCB63' }, danger: '#F0717A' },
  } },
}
