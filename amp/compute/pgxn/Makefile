# This makefile assumes that 'pg_config' is in the path, or is passed in the
# PG_CONFIG variable.
#
# The main makefile calls this when you invoke the `make yb-amp-pg-ext-%`
# target. It passes PG_CONFIG pointing at the selected tree's %/bin/pg_config.
# This is a VPATH build; the current directory is build/pgxn-%, and the path to
# the Makefile is passed with the -f argument.

srcdir = $(dir $(firstword $(MAKEFILE_LIST)))

PG_CONFIG = pg_config

subdirs = yb_amp yb_amp_rmgr yb_amp_walredo yb_amp_utils yb_amp_test_utils

# vendor/ submodules are copied into the build dir and built fresh there, so the
# checkout never holds objects and each build type has its own.
vendor_subdirs = vendor/pgsentinel

.PHONY: install install-compute install-storage $(subdirs) $(vendor_subdirs)
install: $(subdirs) $(vendor_subdirs)
install-compute: yb_amp yb_amp_utils yb_amp_test_utils yb_amp_rmgr $(vendor_subdirs)
install-storage: yb_amp_rmgr yb_amp_walredo

$(subdirs): %:
	mkdir -p $*
	$(MAKE) PG_CONFIG=$(PG_CONFIG) -C $* -f $(abspath $(srcdir)/$@/Makefile) install

# The -build suffix keeps the rm off the checkout if make runs from compute/pgxn.
# `clean` drops objects a build inside the checkout left behind, which cp gives
# fresh mtimes. YB_WERROR= drops pgsentinel's -Werror, which our modules don't use.
$(vendor_subdirs): %:
	@test -f $(abspath $(srcdir)/$@/src/Makefile) || { \
		echo "$(abspath $(srcdir)/$@) is not checked out: run 'make submodules' in the amp root, or build with ./build.sh."; \
		exit 1; }
	rm -rf $*-build
	mkdir -p $*-build
	cp -R $(abspath $(srcdir)/$@/src)/. $*-build
	$(MAKE) PG_CONFIG=$(PG_CONFIG) -C $*-build clean
	$(MAKE) PG_CONFIG=$(PG_CONFIG) YB_WERROR= -C $*-build install
