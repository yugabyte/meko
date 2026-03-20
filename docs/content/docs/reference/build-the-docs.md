---
title: Build the Meko docs
linkTitle: Build the docs
description: How to run the Meko docs locally
weight: 10
---

## Steps

1. Fork the repository (github.com/yugabyte/meko/):

    - Click **Fork**, click **Create Fork**.

1. Create a local clone of your fork:

    - On your machine, open a Terminal window.
    - Navigate to the location where you want the repo files to go.
    - Command:

        ```sh
        git clone https://github.com/<your-github-id>/meko.git
        ```

1. Identify your fork as _origin_ and the original YB repository as _upstream_:

    ```sh
    cd meko
    git remote set-url origin https://github.com/$GITHUB_ID/meko.git
    git remote add upstream https://github.com/yugabyte/meko.git
    ```

1. Make sure that your local repository is still current with the upstream Meko repository:

    ```sh
    cd docs/
    git checkout master
    git pull upstream master
    ```

1. When setting it up for the first time, while in the docs/ directory, run:

    ```sh
    npm install
    ```

1. Start the development server:

    ```sh
    npm run dev
    ```

1. In your browser, navigate to http://localhost:1313/.
