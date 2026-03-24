---
title: Build the Meko docs
linkTitle: Build the docs
description: How to run the Meko docs locally
weight: 10
---

## Using Docker (recommended)

The Docker setup provides a consistent development environment without installing Hugo, Go, or Node.js on your machine.

### Prerequisites

- [Docker Desktop](https://www.docker.com/products/docker-desktop/)

### Quick start

1. Clone the repository:

    ```sh
    git clone https://github.com/yugabyte/meko.git
    cd meko
    ```

1. Start the dev server:

    ```sh
    ./docs-dev.sh start
    ```

1. Visit <http://localhost:1313/> in your browser. Changes to files on your host are reflected automatically via live reload.

### Available commands

| Command | Description |
|---------|-------------|
| `./docs-dev.sh start` | Build image and start Hugo dev server (default) |
| `./docs-dev.sh stop` | Stop the container |
| `./docs-dev.sh shell` | Open a bash shell inside the container |
| `./docs-dev.sh setup-fork` | Configure your GitHub fork and remotes |
| `./docs-dev.sh build` | Run a production build |
| `./docs-dev.sh clean` | Remove containers, volumes, and images |
| `./docs-dev.sh logs` | Tail container logs |

### Setting up your fork for PRs

1. Open a shell in the container:

    ```sh
    ./docs-dev.sh shell
    ```

1. Authenticate with GitHub (if you haven't already):

    ```sh
    gh auth login
    ```

1. Run the fork setup helper:

    ```sh
    exit
    ./docs-dev.sh setup-fork
    ```

1. Create a branch, make your changes on the host, preview in the browser, then push and create a PR from the container shell.

---

## Manual setup (without Docker)

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
    git checkout main
    git pull upstream main
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
