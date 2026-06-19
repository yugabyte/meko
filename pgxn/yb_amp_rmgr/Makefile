# pgxs/yb_amp_rmgr/Makefile


MODULE_big = yb_amp_rmgr
OBJS = \
	$(WIN32RES) \
	neon_rmgr.o \
	neon_rmgr_decode.o \
	neon_rmgr_desc.o


EXTENSION = yb_amp_rmgr
DATA = 
PGFILEDESC = "Neon WAL Resource Manager - custom WAL records used to make Neon work (since PG 16)"


PG_CONFIG = pg_config
PGXS := $(shell $(PG_CONFIG) --pgxs)
include $(PGXS)
