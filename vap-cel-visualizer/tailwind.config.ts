import type { Config } from 'tailwindcss';

export default {
  darkMode: ['class'],
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        ink: '#11100e',
        panel: '#191713',
        panel2: '#211f1a',
        line: '#3c352c',
        mint: '#8aa36f',
        aqua: '#d97757',
        amber: '#d09a4f',
        rose: '#cf6f6b',
      },
      boxShadow: {
        glow: '0 0 34px rgba(217, 119, 87, 0.18)',
      },
      fontFamily: {
        mono: ['JetBrains Mono', 'SFMono-Regular', 'Menlo', 'monospace'],
        sans: ['Inter', 'ui-sans-serif', 'system-ui', 'sans-serif'],
      },
    },
  },
  plugins: [],
} satisfies Config;
