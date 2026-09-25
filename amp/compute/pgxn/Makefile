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

.PHONY: install install-compute install-storage $(subdirs)
install: $(subdirs)
install-compute: yb_amp yb_amp_utils yb_amp_test_utils yb_amp_rmgr
install-storage: yb_amp_rmgr yb_amp_walredo

$(subdirs): %:
	mkdir -p $*
	$(MAKE) PG_CONFIG=$(PG_CONFIG) -C $* -f $(abspath $(srcdir)/$@/Makefile) install
