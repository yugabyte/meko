import { defineConfig } from "eslint/config";
import js from "@eslint/js";
import importPlugin from "eslint-plugin-import";
import stylistic from "@stylistic/eslint-plugin";
import globals from "globals";

export default defineConfig([
  js.configs.recommended,

  {
    files: ["**/*.js", "**/*.mjs"],

    languageOptions: {
      parserOptions: {
        ecmaVersion: "latest",
        sourceType: "module",
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
      "@stylistic": stylistic
    },

    settings: {
      "import/resolver": {
        node: {
          paths: ["./src"]
        }
      }
    },

    rules: {
      // Override / extend recommended rules
      "no-console": "warn",
      "no-debugger": "warn",
      "no-unused-vars": ["warn", { argsIgnorePattern: "^_" }],

      // Import rules
      "import/order": [
        "warn",
        {
          "newlines-between": "always",
          "alphabetize": { order: "asc", caseInsensitive: true }
        }
      ],
      "import/no-unresolved": "error",

      // Stylistic rules
      "@stylistic/semi": ["warn", "always"],
      "@stylistic/quotes": ["warn", "double"],
      "@stylistic/indent": ["warn", 2]
    }
  },

  {
    ignores: ["node_modules", "public"]
  }
]);
