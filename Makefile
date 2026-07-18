ROOT_PROJECT_DIR := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))

# Where to install Postgres, default is ./pg_install, maybe useful for package
# managers.
POSTGRES_INSTALL_DIR ?= $(ROOT_PROJECT_DIR)/pg_install

# Supported PostgreSQL versions
POSTGRES_VERSIONS = v18

# CARGO_BUILD_FLAGS: Extra flags to pass to `cargo build`. `--locked`
# and `--features testing` are popular examples.
#
# CARGO_PROFILE: Set to override the cargo profile to use. By default,
# it is derived from BUILD_TYPE.

# All intermediate build artifacts are stored here.
BUILD_DIR := $(ROOT_PROJECT_DIR)/build

# --- Controller versioning ---------------------------------------------------
# The repo-root VERSION file is the single source of truth for the controller's
# version (a hybrid CalVer, MAJOR.YYMMDD.PATCH, via scripts/version.sh, so the
# binary, image tag, and chart agree). It is stamped into the binary via -ldflags and served at
# GET /api/v1/version. By default a build is a pre-release ("<VERSION>-dev.
# <commit>[-dirty]"); set RELEASE=1 for the clean release version. Override
# AMP_VERSION to force a specific value.
AMP_VERSION ?= $(shell $(ROOT_PROJECT_DIR)/scripts/version.sh binary-version)
AMP_GIT_COMMIT ?= $(shell git -C $(ROOT_PROJECT_DIR) rev-parse --short HEAD 2>/dev/null || echo unknown)
AMP_BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
CONTROLLER_VERSION_PKG := github.com/yugabyte/amp/controller/internal/version
CONTROLLER_LDFLAGS := -X '$(CONTROLLER_VERSION_PKG).Version=$(AMP_VERSION)' -X '$(CONTROLLER_VERSION_PKG).Commit=$(AMP_GIT_COMMIT)' -X '$(CONTROLLER_VERSION_PKG).BuildDate=$(AMP_BUILD_DATE)'

ICU_PREFIX_DIR := /usr/local/icu

#
# We differentiate between release / debug build types using the BUILD_TYPE
# environment variable.
#
BUILD_TYPE ?= debug
WITH_SANITIZERS ?= no
PG_CFLAGS = -fsigned-char
ifeq ($(BUILD_TYPE),release)
	PG_CONFIGURE_OPTS = --enable-debug --with-openssl
	PG_CFLAGS += -O2 -g3 $(CFLAGS)
	PG_LDFLAGS = $(LDFLAGS)
	CARGO_PROFILE ?= --profile=release
	# YBAMP_CARGO_ARTIFACT_TARGET_DIR is the directory where `cargo build` places
	# the final build artifacts. There is unfortunately no easy way of changing
	# it to a fully predictable path, nor to extract the path with a simple
	# command. See https://github.com/rust-lang/cargo/issues/9661 and
	# https://github.com/rust-lang/cargo/issues/6790.
	YBAMP_CARGO_ARTIFACT_TARGET_DIR = $(ROOT_PROJECT_DIR)/target/release
else ifeq ($(BUILD_TYPE),debug)
	PG_CONFIGURE_OPTS = --enable-debug --with-openssl --enable-cassert --enable-depend
	PG_CFLAGS += -O0 -g3 $(CFLAGS)
	PG_LDFLAGS = $(LDFLAGS)
	CARGO_PROFILE ?= --profile=dev
	YBAMP_CARGO_ARTIFACT_TARGET_DIR = $(ROOT_PROJECT_DIR)/target/debug
else
	$(error Bad build type '$(BUILD_TYPE)', see Makefile for options)
endif

ifeq ($(WITH_SANITIZERS),yes)
	PG_CFLAGS += -fsanitize=address -fsanitize=undefined -fno-sanitize-recover
	COPT += -Wno-error # to avoid failing on warnings induced by sanitizers
	PG_LDFLAGS = -fsanitize=address -fsanitize=undefined -static-libasan -static-libubsan $(LDFLAGS)
	export CC := gcc
	export ASAN_OPTIONS := detect_leaks=0
endif

ifeq ($(shell test -e /home/nonroot/.docker_build && echo -n yes),yes)
	# Exclude static build openssl, icu for local build (MacOS, Linux)
	# Only keep for build type release and debug
	PG_CONFIGURE_OPTS += --with-icu
	PG_CONFIGURE_OPTS += ICU_CFLAGS='-I/$(ICU_PREFIX_DIR)/include -DU_STATIC_IMPLEMENTATION'
	PG_CONFIGURE_OPTS += ICU_LIBS='-L$(ICU_PREFIX_DIR)/lib -L$(ICU_PREFIX_DIR)/lib64 -licui18n -licuuc -licudata -lstdc++ -Wl,-Bdynamic -lm'
endif

UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Linux)
	# Seccomp BPF is only available for Linux
	ifneq ($(WITH_SANITIZERS),yes)
		PG_CONFIGURE_OPTS += --with-libseccomp
	endif
else ifeq ($(UNAME_S),Darwin)
	PG_CFLAGS += -DUSE_PREFETCH
	ifndef DISABLE_HOMEBREW
		# macOS with brew-installed openssl requires explicit paths
		# It can be configured with OPENSSL_PREFIX variable
		OPENSSL_PREFIX := $(shell brew --prefix openssl@3)
		PG_CONFIGURE_OPTS += --with-includes=$(OPENSSL_PREFIX)/include --with-libraries=$(OPENSSL_PREFIX)/lib
		PG_CONFIGURE_OPTS += PKG_CONFIG_PATH=$(shell brew --prefix icu4c)/lib/pkgconfig
		# macOS already has bison and flex in the system, but they are old and result in postgres-v14 target failure
		# brew formulae are keg-only and not symlinked into HOMEBREW_PREFIX, force their usage
		EXTRA_PATH_OVERRIDES += $(shell brew --prefix bison)/bin/:$(shell brew --prefix flex)/bin/:
	endif
endif

# Use -C option so that when PostgreSQL "make install" installs the
# headers, the mtime of the headers are not changed when there have
# been no changes to the files. Changing the mtime triggers an
# unnecessary rebuild of 'postgres_ffi'.
PG_CONFIGURE_OPTS += INSTALL='$(ROOT_PROJECT_DIR)/scripts/ninstall.sh -C'

# Choose whether we should be silent or verbose
CARGO_BUILD_FLAGS += --$(if $(filter s,$(MAKEFLAGS)),quiet,verbose)
# Fix for a corner case when make doesn't pass a jobserver
CARGO_BUILD_FLAGS += $(filter -j1,$(MAKEFLAGS))

# This option has a side effect of passing make jobserver to cargo.
# However, we shouldn't do this if `make -n` (--dry-run) has been asked.
CARGO_CMD_PREFIX += $(if $(filter n,$(MAKEFLAGS)),,+)
# Force cargo not to print progress bar
CARGO_CMD_PREFIX += CARGO_TERM_PROGRESS_WHEN=never CI=1

CACHEDIR_TAG_CONTENTS := "Signature: 8a477f597d28d172789f06886806bc55"

#
# Top level Makefile to build ybAmp and PostgreSQL
#
.PHONY: all
all: yb-amp postgres-install yb-amp-pg-ext

### ybAmp Rust bits
#
# The 'postgres_ffi' crate depends on the Postgres headers.
.PHONY: yb-amp
yb-amp: postgres-headers-install walproposer-lib cargo-target-dir
	+@echo "Compiling ybAmp"
	$(CARGO_CMD_PREFIX) cargo build $(CARGO_BUILD_FLAGS) $(CARGO_PROFILE)

.PHONY: cargo-target-dir
cargo-target-dir:
	# https://github.com/rust-lang/cargo/issues/14281
	mkdir -p target
	test -e target/CACHEDIR.TAG || echo "$(CACHEDIR_TAG_CONTENTS)" > target/CACHEDIR.TAG

.PHONY: yb-amp-pg-ext-%
yb-amp-pg-ext-%: postgres-install-% cargo-target-dir
	+@echo "Compiling yb_amp Postgres extensions for $*"
	mkdir -p $(BUILD_DIR)/pgxn-$*
	$(MAKE) PG_CONFIG="$(POSTGRES_INSTALL_DIR)/$*/bin/pg_config" COPT='$(COPT)' \
		YBAMP_CARGO_ARTIFACT_TARGET_DIR="$(YBAMP_CARGO_ARTIFACT_TARGET_DIR)" \
		CARGO_BUILD_FLAGS="$(CARGO_BUILD_FLAGS)" \
		CARGO_PROFILE="$(CARGO_PROFILE)" \
		-C $(BUILD_DIR)/pgxn-$*\
		-f $(ROOT_PROJECT_DIR)/compute/pgxn/Makefile  install

# Build walproposer as a static library. walproposer source code is located
# in the compute/pgxn/yb_amp directory.
#
# We also need to include libpgport.a and libpgcommon.a, because walproposer
# uses some functions from those libraries.
#
# Some object files are removed from libpgport.a and libpgcommon.a because
# they depend on openssl and other libraries that are not included in our
# Rust build.
.PHONY: walproposer-lib
walproposer-lib: yb-amp-pg-ext-v18
	+@echo "Compiling walproposer-lib"
	mkdir -p $(BUILD_DIR)/walproposer-lib
	$(MAKE) PG_CONFIG=$(POSTGRES_INSTALL_DIR)/v18/bin/pg_config COPT='$(COPT)' \
		-C $(BUILD_DIR)/walproposer-lib \
		-f $(ROOT_PROJECT_DIR)/compute/pgxn/yb_amp/Makefile walproposer-lib
	cp $(POSTGRES_INSTALL_DIR)/v18/lib/libpgport.a $(BUILD_DIR)/walproposer-lib
	cp $(POSTGRES_INSTALL_DIR)/v18/lib/libpgcommon.a $(BUILD_DIR)/walproposer-lib
	$(AR) d $(BUILD_DIR)/walproposer-lib/libpgport.a \
		pg_strong_random.o
	$(AR) d $(BUILD_DIR)/walproposer-lib/libpgcommon.a \
		checksum_helper.o \
		cryptohash_openssl.o \
		hmac_openssl.o \
		md5_common.o \
		parse_manifest.o \
		scram-common.o
ifeq ($(UNAME_S),Linux)
	$(AR) d $(BUILD_DIR)/walproposer-lib/libpgcommon.a \
		pg_crc32c.o
endif

# Shorthand to call yb-amp-pg-ext-% target for all Postgres versions
.PHONY: yb-amp-pg-ext
yb-amp-pg-ext: $(foreach pg_version,$(POSTGRES_VERSIONS),yb-amp-pg-ext-$(pg_version))

### amp controller (Go control plane)
#
# The amp controller is a self-contained Go module under controller/, independent of
# the Rust/C build above (and deliberately not part of `all`).
.PHONY: controller
controller:
	+@echo "Building amp controller"
	go -C controller build ./...

.PHONY: controller-test
controller-test:
	go -C controller test ./...

.PHONY: controller-vet
controller-vet:
	go -C controller vet ./...

# Chart tests for the compute (yb-amp-endpoint) chart: the helm-unittest
# suites plus the Guaranteed-QoS guardrail, which asserts every container in the
# compute pod sets resources.requests == resources.limits (needs helm + yq).
.PHONY: endpoint-chart-test
endpoint-chart-test:  ## helm-unittest + Guaranteed-QoS guardrail for yb-amp-endpoint
	helm unittest deploy/helm/yb-amp-endpoint
	bash deploy/helm/yb-amp-endpoint/tests/guaranteed-qos.sh

# Regenerate the API code (models, echo server, Go client) from the OpenAPI
# spec (controller/internal/api/openapi_spec.yaml). Run after editing the spec; the
# generated api.gen.go is checked in. Uses the oapi-codegen tool pinned in
# controller/go.mod.
.PHONY: controller-generate
controller-generate:
	go -C controller generate ./...

# Bundle the amp controller and the controller it manages, side by side
# in one directory, so controller resolves and runs the controller from its own
# location with no extra configuration (#94).
#
# The controller is a pure-Go binary (CGO off), so this needs only the Go
# toolchain — no Rust, no Postgres headers, no compute/vendor/postgres-v18
# submodule. The deploy/controller container packages the same layout.
# Output: $(CONTROLLER_BUNDLE_DIR)/controller.
CONTROLLER_BUNDLE_DIR ?= $(ROOT_PROJECT_DIR)/target/controller-bundle

# Initialize submodules (currently just compute/vendor/postgres-v18). Idempotent: a no-op
# when they are already present and at the recorded commit, so it is safe to run
# on every build.
.PHONY: submodules
submodules:
	git submodule update --init --recursive --depth 2 --progress .

# controller-bundle initializes the submodule before building so the postgres-headers
# step finds compute/vendor/postgres-v18. The sub-make keeps that ordered ahead of the
# headers build even under parallel make (-j), where listing it as a sibling
# prerequisite would race the postgres configure check.
# --- versioning targets (thin wrappers over scripts/version.sh) ---
.PHONY: version version-image-tag version-sync version-bump-patch version-bump-minor version-bump-major
version:            ## print the current semantic version
	@$(ROOT_PROJECT_DIR)/scripts/version.sh
version-image-tag:  ## print the container image tag (v<VERSION>)
	@$(ROOT_PROJECT_DIR)/scripts/version.sh image-tag
version-sync:       ## stamp Chart.yaml version/appVersion from the VERSION file
	@$(ROOT_PROJECT_DIR)/scripts/version.sh sync
version-bump-patch: ## re-cut the current date: bump the same-day PATCH and sync the chart
	@$(ROOT_PROJECT_DIR)/scripts/version.sh bump patch
version-bump-minor: ## roll the CalVer date (YYMMDD) to today and reset PATCH, then sync the chart
	@$(ROOT_PROJECT_DIR)/scripts/version.sh bump date
version-bump-major: ## bump the MAJOR epoch (re-stamps the date, resets PATCH) and sync the chart
	@$(ROOT_PROJECT_DIR)/scripts/version.sh bump major

.PHONY: controller-bundle
controller-bundle:
	+$(MAKE) submodules
	+$(MAKE) controller-bundle-build

.PHONY: controller-bundle-build
controller-bundle-build:
	+@echo "Bundling controller -> $(CONTROLLER_BUNDLE_DIR)"
	mkdir -p "$(CONTROLLER_BUNDLE_DIR)"
	# controller is built static (CGO off) so it runs unchanged in a slim runtime image.
	CGO_ENABLED=0 go -C controller build -trimpath -ldflags "$(CONTROLLER_LDFLAGS)" -o "$(CONTROLLER_BUNDLE_DIR)/controller" ./cmd/controller

# controller-only bundle: just the Go controller, no controller. Needs no Rust
# toolchain, no Postgres headers, and no compute/vendor/postgres-v18 submodule — so it
# builds in environments without the Rust toolchain. Used by the docker integration (controller/tasks).
.PHONY: controller-bundle-only
controller-bundle-only:
	+@echo "Bundling controller (Go only, no controller) -> $(CONTROLLER_BUNDLE_DIR)"
	$(RM) -r "$(CONTROLLER_BUNDLE_DIR)"
	mkdir -p "$(CONTROLLER_BUNDLE_DIR)"
	CGO_ENABLED=0 go -C controller build -trimpath -ldflags "$(CONTROLLER_LDFLAGS)" -o "$(CONTROLLER_BUNDLE_DIR)/controller" ./cmd/controller

# This removes everything
.PHONY: distclean
distclean:
	$(RM) -r $(POSTGRES_INSTALL_DIR) $(BUILD_DIR)
	$(CARGO_CMD_PREFIX) cargo clean

.PHONY: fmt
fmt:
	./pre-commit.py --fix-inplace

postgres-%-pg-bsd-indent: postgres-%
	+@echo "Compiling pg_bsd_indent"
	$(MAKE) -C $(BUILD_DIR)/$*/src/tools/pg_bsd_indent/

# Create typedef list for the core. Note that generally it should be combined with
# buildfarm one to cover platform specific stuff.
# https://wiki.postgresql.org/wiki/Running_pgindent_on_non-core_code_or_development_code
postgres-%-typedefs.list: postgres-%
	$(ROOT_PROJECT_DIR)/compute/vendor/postgres-$*/src/tools/find_typedef $(POSTGRES_INSTALL_DIR)/$*/bin > $@

# Indent postgres. See src/tools/pgindent/README for details.
.PHONY: postgres-%-pgindent
postgres-%-pgindent: postgres-%-pg-bsd-indent postgres-%-typedefs.list
	+@echo merge with buildfarm typedef to cover all platforms
	+@echo note: I first tried to download from pgbuildfarm.org, but for unclear reason e.g. \
		REL_16_STABLE list misses PGSemaphoreData
	# wget -q -O - "http://www.pgbuildfarm.org/cgi-bin/typedefs.pl?branch=REL_16_STABLE" |\
	# cat - postgres-$*-typedefs.list | sort | uniq > postgres-$*-typedefs-full.list
	cat $(ROOT_PROJECT_DIR)/compute/vendor/postgres-$*/src/tools/pgindent/typedefs.list |\
		cat - postgres-$*-typedefs.list | sort | uniq > postgres-$*-typedefs-full.list
	+@echo note: you might want to run it on selected files/dirs instead.
	INDENT=$(BUILD_DIR)/$*/src/tools/pg_bsd_indent/pg_bsd_indent \
		$(ROOT_PROJECT_DIR)/compute/vendor/postgres-$*/src/tools/pgindent/pgindent --typedefs postgres-$*-typedefs-full.list \
		$(ROOT_PROJECT_DIR)/compute/vendor/postgres-$*/src/ \
		--excludes $(ROOT_PROJECT_DIR)/compute/vendor/postgres-$*/src/tools/pgindent/exclude_file_patterns
	$(RM) pg*.BAK

# Indent pgxn/yb_amp.
.PHONY: yb-amp-pgindent
yb-amp-pgindent: postgres-v18-pg-bsd-indent yb-amp-pg-ext-v18
	$(MAKE) PG_CONFIG=$(POSTGRES_INSTALL_DIR)/v18/bin/pg_config COPT='$(COPT)' \
		FIND_TYPEDEF=$(ROOT_PROJECT_DIR)/compute/vendor/postgres-v18/src/tools/find_typedef \
		INDENT=$(BUILD_DIR)/v18/src/tools/pg_bsd_indent/pg_bsd_indent \
		PGINDENT_SCRIPT=$(ROOT_PROJECT_DIR)/compute/vendor/postgres-v18/src/tools/pgindent/pgindent \
		-C $(BUILD_DIR)/pgxn-v18/yb_amp \
		-f $(ROOT_PROJECT_DIR)/compute/pgxn/yb_amp/Makefile pgindent


.PHONY: setup-pre-commit-hook
setup-pre-commit-hook:
	ln -s -f $(ROOT_PROJECT_DIR)/pre-commit.py .git/hooks/pre-commit

build-tools/node_modules: build-tools/package.json
	cd build-tools && $(if $(CI),npm ci,npm install)
	touch build-tools/node_modules

.PHONY: lint-openapi-spec
lint-openapi-spec: build-tools/node_modules
	# operation-2xx-response: pageserver timeline delete returns 404 on success
	find . -iname "openapi_spec.y*ml" -exec\
		npx --prefix=build-tools/ redocly\
			--skip-rule=operation-operationId --skip-rule=operation-summary --extends=minimal\
			--skip-rule=no-server-example.com --skip-rule=operation-2xx-response\
			lint {} \+

# Targets for building PostgreSQL are defined in postgres.mk.
#
# But if the caller has indicated that PostgreSQL is already
# installed, by setting the PG_INSTALL_CACHED variable, skip it.
ifdef PG_INSTALL_CACHED
postgres-install: skip-install
$(foreach pg_version,$(POSTGRES_VERSIONS),postgres-install-$(pg_version)): skip-install
postgres-headers-install:
	+@echo "Skipping installation of PostgreSQL headers because PG_INSTALL_CACHED is set"
skip-install:
	+@echo "Skipping PostgreSQL installation because PG_INSTALL_CACHED is set"

else
include postgres.mk
endif
