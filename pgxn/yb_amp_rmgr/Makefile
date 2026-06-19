# pgxs/yb_amp_rmgr/Makefile


MODULE_big = yb_amp_rmgr
OBJS = \
	$(WIN32RES) \
	yb_amp_rmgr.o \
	yb_amp_rmgr_decode.o \
	yb_amp_rmgr_desc.o


EXTENSION = yb_amp_rmgr
DATA = 
PGFILEDESC = "YbAmp WAL Resource Manager - custom WAL records used to make YbAmp work (since PG 16)"


PG_CONFIG = pg_config
PGXS := $(shell $(PG_CONFIG) --pgxs)
include $(PGXS)
