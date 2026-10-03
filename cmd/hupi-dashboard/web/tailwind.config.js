/** @type {import('tailwindcss').Config} */
// Color tokens ported directly from the marketing site's design system
// (site/src/styles/global.css's @theme block) — same navy/fog/ember
// palette, just expressed as Tailwind v3 theme.extend.colors since this
// app is still on the PostCSS config path, not the site's Tailwind v4
// CSS-first @theme. accent is kept as an alias for ember so existing
// `accent`-referencing classes (none currently, but written against this
// config) keep working if ever used.
export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        ink: "#05060a",
        navy: {
          950: "#070912",
          900: "#0b0e19",
          800: "#10141f",
          700: "#171c2b",
          600: "#232a3d",
          500: "#333c54",
        },
        fog: {
          100: "#f5f6fa",
          300: "#cbd0dd",
          500: "#8991a8",
          700: "#5b637a",
        },
        ember: {
          400: "#ffb073",
          500: "#ff8a4c",
          600: "#f0692a",
        },
        accent: {
          DEFAULT: "#ff8a4c",
          hover: "#ffb073",
        },
      },
      fontFamily: {
        sans: ["Inter", "ui-sans-serif", "system-ui", "sans-serif"],
        mono: ["JetBrains Mono", "ui-monospace", "monospace"],
      },
    },
  },
  plugins: [],
};
