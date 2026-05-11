import type { Config } from 'tailwindcss';

export default {
  darkMode: ['class'],
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        ink: '#080b10',
        panel: '#0d121a',
        panel2: '#121925',
        line: '#273243',
        mint: '#4ade80',
        aqua: '#22d3ee',
        amber: '#fbbf24',
        rose: '#fb7185',
      },
      boxShadow: {
        glow: '0 0 28px rgba(34, 211, 238, 0.18)',
      },
      fontFamily: {
        mono: ['JetBrains Mono', 'SFMono-Regular', 'Menlo', 'monospace'],
        sans: ['Inter', 'ui-sans-serif', 'system-ui', 'sans-serif'],
      },
    },
  },
  plugins: [],
} satisfies Config;
