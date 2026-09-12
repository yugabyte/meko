// meko image build: builds the component images from their source repos and pushes them
// to quay under one version tag (release: tag-driven; custom: test builds), in parallel on
// ephemeral AWS builder instances.
// Registered as users/shubin/meko-devops/MEKO/meko-build (Pipeline script from SCM
// → yugabyte/meko, script path meko/jenkins/pipelines/pipeline_meko_build.groovy).

pipeline {
    agent {
        docker {
            label 'amp-agent'
            image 'ghcr.io/yugabyte/meko-devops/amp-jenkins-agent:0.3'
            registryUrl 'https://ghcr.io'
            registryCredentialsId 'stress-docker-test'
            args '-u root'
        }
    }

    options {
        timestamps()
        ansiColor('xterm')
        buildDiscarder(logRotator(numToKeepStr: '30', artifactNumToKeepStr: '30'))
        disableConcurrentBuilds()
        timeout(time: 3, unit: 'HOURS')
    }

    parameters {
        choice(name: 'MODE', choices: ['auto', 'release', 'custom'],
               description: 'release: if product code changed since the last release tag, compute 0.YYMMDD.PATCH on the fly, publish <version>-<short-sha> and push the git tag (RELEASE_TAG_PREFIX + version). custom: tag custom-<short-sha> (test runs), skipped when already on quay. auto: release for a parameterless main build, custom otherwise.')
        string(name: 'BRANCH', defaultValue: 'main',
               description: 'meko branch to build')
        string(name: 'COMMIT', defaultValue: '',
               description: 'exact sha on BRANCH (empty = branch head)')
        string(name: 'IMAGE_VERSION', defaultValue: '',
               description: 'quay tag for all images; empty = custom-<short-sha> of the built commit')
        string(name: 'IMAGES', defaultValue: 'api-server,ui,inference-gateway,provisioner,mcp-server,rag-agent,langfuse-web,langfuse-worker',
               description: 'which images to build (CSV) — one AWS builder instance each, in parallel, except the langfuse pair which shares one. A tag already on quay is skipped and launches nothing.')
        string(name: 'MCP_REF', defaultValue: 'main',
               description: 'yugabyte/meko-mcp-server branch or sha — mcp-server')
        string(name: 'LANGFUSE_REF', defaultValue: 'main',
               description: 'yugabyte/meko-langfuse branch or sha — langfuse-web, langfuse-worker')
        string(name: 'YBDB_REF', defaultValue: 'master',
               description: 'yugabyte/yugabyte-db branch or sha — rag-agent')
        booleanParam(name: 'PUSH', defaultValue: true,
               description: 'untick to build only, without pushing to quay')
        string(name: 'INSTANCE_TYPE', defaultValue: 'c7i.2xlarge',
               description: 'EC2 type for the builder instances')
        string(name: 'BUILDER_AMI', defaultValue: '',
               description: 'builder AMI id; empty = script default ami-044fea1e64cb7e8ee (packer: meko-devops jenkins/amp/builder-ami)')
        string(name: 'AWS_CREDENTIALS_ID', defaultValue: '28669ca6-7fb9-489d-82c4-be9fa7104d9b',
               description: 'Jenkins AWS credential for launching/terminating the builder instances; empty = agent ambient identity')
        string(name: 'MEKO_REPO', defaultValue: 'git@github.com:yugabyte/meko.git',
               description: 'repo to build')
        string(name: 'RELEASE_TAG_PREFIX', defaultValue: 'meko-',
               description: 'git tag namespace for releases, e.g. meko-0.260911.0. The monorepo holds several independently released components, so a bare 0.* tag would read as the whole repo\'s version.')
        string(name: 'build_meta', defaultValue: '',
               description: 'queue discriminator so identical parameter sets are not merged by Jenkins')
    }

    environment {
        MANIFEST = "${WORKSPACE}/meko-build-manifest.txt"
    }

    stages {
        // The container runs as root; anything it leaves in the workspace is
        // undeletable by the checkout, which runs outside the container.
        stage('Scrub workspace') {
            steps {
                sh 'rm -rf "$WORKSPACE"/.meko-build "$WORKSPACE"/meko-build-manifest.txt'
            }
        }

        stage('Checkout') {
            steps {
                checkout([$class: 'GitSCM',
                          branches: [[name: "*/${params.BRANCH}"]],
                          extensions: [[$class: 'CleanBeforeCheckout'],
                                       [$class: 'CloneOption', shallow: false, noTags: false]],
                          userRemoteConfigs: [[url: params.MEKO_REPO,
                                               credentialsId: 'jenkins-user-key-vcs']]])
                script {
                    if (params.COMMIT?.trim()) {
                        sshagent(credentials: ['jenkins-user-key-vcs']) {
                            sh '''#!/bin/bash -e
                                export GIT_SSH_COMMAND='ssh -o StrictHostKeyChecking=accept-new'
                                git cat-file -e "$COMMIT^{commit}" 2>/dev/null || git fetch --no-tags origin "$COMMIT"
                                git checkout --detach "$COMMIT"
                            '''
                        }
                    }
                    env.MEKO_SHA = sh(script: 'git rev-parse --short HEAD', returnStdout: true).trim()
                    def mode = params.MODE
                    if (mode == 'auto') {
                        mode = (!params.COMMIT?.trim() && !params.IMAGE_VERSION?.trim() &&
                                params.BRANCH == 'main') ? 'release' : 'custom'
                    }
                    env.BUILD_MODE = mode
                    if (mode == 'release') {
                        env.LAST_RELEASE_TAG = sh(returnStdout: true, script:
                            "git describe --tags --abbrev=0 --match '${params.RELEASE_TAG_PREFIX}[0-9]*.[0-9]*.[0-9]*' 2>/dev/null || true").trim()
                        // Denylist, not allowlist: everything under meko/ is product unless
                        // it is a test or a document, so a new top-level directory is
                        // released by default and has to be excluded on purpose. amp learned
                        // this the hard way — an allowlist missed deploy/helm/ and shipped a
                        // real chart change unreleased.
                        //
                        // Only ^meko/ counts, so amp and ycp changes alongside it never
                        // trigger a meko release; they have their own pipelines. meko/jenkins/
                        // is this pipeline itself and ships in no image. Changes in
                        // meko-mcp-server / meko-langfuse / yugabyte-db do not trigger one
                        // either — pass MODE=release explicitly to publish those.
                        def prod = sh(returnStdout: true, script: '''#!/bin/bash
                            if [ -n "$LAST_RELEASE_TAG" ]; then
                                git diff --name-only "$LAST_RELEASE_TAG"..HEAD
                            else
                                git ls-files
                            fi 2>/dev/null \
                                | grep -E '^meko/' \
                                | grep -Ev '^meko/(docs/|runbooks/|design/|jenkins/)' \
                                | grep -Ev '\\.md$|_test\\.go$|(^|/)tests?/|/testdata/' \
                                || true
                        ''').trim()
                        if (!prod) {
                            env.SKIP_BUILD = 'true'
                            currentBuild.description = "no product changes since ${env.LAST_RELEASE_TAG} — skipped"
                            echo "no product changes since the last release tag (${env.LAST_RELEASE_TAG}); nothing to release"
                        } else {
                            env.RELEASE_BASE = sh(returnStdout: true, script: '''#!/bin/bash -e
                                DATE=$(git show -s --format=%cd --date=format:%y%m%d HEAD)
                                LAST_PATCH=$(git tag -l "${RELEASE_TAG_PREFIX}0.${DATE}.*" \
                                    | sed "s|^${RELEASE_TAG_PREFIX}||" | awk -F. '{print $3}' | sort -n | tail -1)
                                if [ -n "$LAST_PATCH" ]; then PATCH=$((LAST_PATCH + 1)); else PATCH=0; fi
                                echo "0.${DATE}.${PATCH}"
                            ''').trim()
                            env.RELEASE_TAG = params.RELEASE_TAG_PREFIX + env.RELEASE_BASE
                            env.VERSION = env.RELEASE_BASE + '-' + env.MEKO_SHA
                            echo "release ${env.VERSION} (product changes since ${env.LAST_RELEASE_TAG ?: 'the beginning'}):\n${prod}"
                        }
                    } else {
                        env.VERSION = params.IMAGE_VERSION?.trim() ?: "custom-${env.MEKO_SHA}"
                    }
                    if (env.SKIP_BUILD != 'true') {
                        currentBuild.description = "${env.VERSION} (${params.BRANCH}@${env.MEKO_SHA}, ${mode})"
                    }
                }
            }
        }

        stage('Build on AWS (parallel) & push to quay') {
            when { expression { env.SKIP_BUILD != 'true' } }
            steps {
                script {
                    def orchestrate = {
                        sshagent(credentials: ['aws-ssh-key', 'jenkins-user-key-vcs']) {
                            withCredentials([usernamePassword(credentialsId: 'quay-mekobot',
                                                              usernameVariable: 'QUAY_USER',
                                                              passwordVariable: 'QUAY_TOKEN')]) {
                                sh '''#!/bin/bash -e
                                    export GIT_SSH_COMMAND='ssh -o StrictHostKeyChecking=accept-new'
                                    export VERSION="$VERSION" IMAGES="$IMAGES"
                                    export MEKO_REPO="$MEKO_REPO" MEKO_REF="$(git rev-parse HEAD)"
                                    export MCP_REF="$MCP_REF" LANGFUSE_REF="$LANGFUSE_REF" YBDB_REF="$YBDB_REF"
                                    export PUSH=$([ "$PUSH" = true ] && echo 1 || echo 0)
                                    export INSTANCE_TYPE="$INSTANCE_TYPE" BUILD_LABEL="$BUILD_NUMBER"
                                    export MANIFEST="$MANIFEST"
                                    [ -n "$BUILDER_AMI" ] && export BUILDER_AMI="$BUILDER_AMI"
                                    bash meko/jenkins/scripts/build-images-aws.sh
                                '''
                            }
                        }
                    }
                    if (params.AWS_CREDENTIALS_ID?.trim()) {
                        withAWS(credentials: params.AWS_CREDENTIALS_ID) { orchestrate() }
                    } else {
                        orchestrate()
                    }
                }
            }
        }

        stage('Tag release commit') {
            // Without PUSH the build publishes no images, and a tag with nothing
            // behind it becomes the next run's LAST_RELEASE_TAG — the commit it
            // names would then never be released.
            when { expression { env.BUILD_MODE == 'release' && env.RELEASE_BASE?.trim() && params.PUSH } }
            steps {
                sshagent(credentials: ['jenkins-user-key-vcs']) {
                    sh '''#!/bin/bash -e
                        export GIT_SSH_COMMAND='ssh -o StrictHostKeyChecking=accept-new'
                        if git ls-remote --exit-code --tags origin "refs/tags/$RELEASE_TAG" >/dev/null 2>&1; then
                            echo "$RELEASE_TAG already exists on origin"
                        else
                            git -c user.name='meko-build' -c user.email='jenkins@yugabyte.com' \
                                tag -a "$RELEASE_TAG" -m "meko $RELEASE_BASE"
                            git push origin "$RELEASE_TAG"
                        fi
                    '''
                }
            }
        }
    }

    post {
        always {
            script {
                if (fileExists(env.MANIFEST)) {
                    archiveArtifacts artifacts: 'meko-build-manifest.txt', allowEmptyArchive: true
                    echo readFile(env.MANIFEST)
                }
            }
        }
    }
}
