import js from '@eslint/js';
import tseslint from 'typescript-eslint';
import globals from 'globals';
import reactHooks from 'eslint-plugin-react-hooks';

export default tseslint.config(
  { ignores: ['dist/**', 'node_modules/**', 'coverage/**'] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    // Server, scripts, tests and config files run in Node.
    languageOptions: { globals: globals.node },
    rules: {
      '@typescript-eslint/no-unused-vars': ['error', { argsIgnorePattern: '^_', varsIgnorePattern: '^_' }],
      'no-empty': ['error', { allowEmptyCatch: true }],
    },
  },
  {
    // The web UI runs in the browser: no Node globals there, and browser globals only there.
    files: ['src/web/**'],
    languageOptions: { globals: globals.browser },
  },
  {
    // The classic hooks rules. The plugin's React Compiler rules (refs read in render, setState
    // in effects…) are left off: this app doesn't use the compiler, and they flag deliberate
    // patterns such as keeping the latest callback in a ref.
    files: ['src/web/**/*.{ts,tsx}'],
    plugins: { 'react-hooks': reactHooks },
    rules: {
      'react-hooks/rules-of-hooks': 'error',
      'react-hooks/exhaustive-deps': 'error',
    },
  },
  {
    files: ['src/web/public/sw.js'],
    languageOptions: { globals: globals.serviceworker },
  },
  {
    // Tests of web code run in Node but exercise browser-facing modules.
    files: ['test/**'],
    languageOptions: { globals: { ...globals.node, ...globals.browser } },
  },
);
