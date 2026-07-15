import react from '@vitejs/plugin-react';
import { defineConfig } from 'vite';

// base './' so the built site works on GitHub Pages under any path.
export default defineConfig({
  plugins: [react()],
  base: './',
});
