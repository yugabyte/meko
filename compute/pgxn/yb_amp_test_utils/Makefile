# pgxs/yb_amp_test_utils/Makefile


MODULE_big = yb_amp_test_utils
OBJS = \
	$(WIN32RES) \
	yb_amptest.o

EXTENSION = yb_amp_test_utils
DATA = yb_amp_test_utils--1.3.sql
PGFILEDESC = "yb_amp_test_utils - helpers for yb_amp testing and debugging"

PG_CONFIG = pg_config
PGXS := $(shell $(PG_CONFIG) --pgxs)
include $(PGXS)
