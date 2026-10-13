import { defineConfig } from 'eslint/config';
import js from '@eslint/js';
import importPlugin from 'eslint-plugin-import';
import globals from 'globals';

export default defineConfig([
  js.configs.recommended,

  {
    files: ['**/*.js', '**/*.mjs'],

    languageOptions: {
      parserOptions: {
        ecmaVersion: 'latest',
        sourceType: 'module',
        ecmaFeatures: {
          jsx: true,
        },
      },
      globals: {
        ...globals.browser,
      },
    },

    plugins: {
      import: importPlugin,
    },

    settings: {
      'import/resolver': {
        node: {
          paths: ['./src'],
        },
      },
    },

    // Formatting (quotes, semicolons, indentation) is Prettier's, via .prettierrc.
    rules: {
      // Override / extend recommended rules
      'no-console': 'warn',
      'no-debugger': 'warn',
      'no-unused-vars': ['warn', { argsIgnorePattern: '^_' }],

      // Import rules
      'import/order': [
        'warn',
        {
          'newlines-between': 'always',
          alphabetize: { order: 'asc', caseInsensitive: true },
        },
      ],
      'import/no-unresolved': 'error',
    },
  },

  {
    files: ['*.js', '*.mjs', 'netlify/**/*.mjs'],
    languageOptions: {
      globals: {
        ...globals.node,
      },
    },
  },

  {
    files: ['webpack.config.js'],
    languageOptions: {
      sourceType: 'commonjs',
    },
  },

  {
    ignores: ['node_modules', 'public'],
  },
]);
