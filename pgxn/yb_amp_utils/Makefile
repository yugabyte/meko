# pgxs/yb_amp_utils/Makefile


MODULE_big = yb_amp_utils
OBJS = \
	$(WIN32RES) \
	neon_utils.o

EXTENSION = yb_amp_utils
DATA = yb_amp_utils--1.0.sql
PGFILEDESC = "yb_amp_utils - small useful functions"

PG_CONFIG = pg_config
PGXS := $(shell $(PG_CONFIG) --pgxs)
include $(PGXS)
