import js from '@eslint/js';
import tseslint from 'typescript-eslint';
import globals from 'globals';
import reactHooks from 'eslint-plugin-react-hooks';

export default tseslint.config(
  { ignores: ['dist/**', 'node_modules/**', 'coverage/**', '.dev-home/**'] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    // Server, tests and config files run in Node.
    languageOptions: { globals: globals.node },
    rules: {
      '@typescript-eslint/no-unused-vars': ['error', { argsIgnorePattern: '^_', varsIgnorePattern: '^_' }],
      'no-empty': ['error', { allowEmptyCatch: true }],
    },
  },
  {
    // The pages run in the browser: no Node globals there.
    files: ['src/web/**'],
    languageOptions: { globals: globals.browser },
  },
  {
    files: ['src/web/**/*.{ts,tsx}'],
    plugins: { 'react-hooks': reactHooks },
    rules: {
      'react-hooks/rules-of-hooks': 'error',
      'react-hooks/exhaustive-deps': 'error',
    },
  },
  {
    files: ['test/**'],
    languageOptions: { globals: { ...globals.node, ...globals.browser } },
  },
);
