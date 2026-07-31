#ifndef YBAMP_LWLSNCACHE_H
#define YBAMP_LWLSNCACHE_H

#include "yb_amp_pgversioncompat.h"

void init_lwlsncache(void);

/* Hooks */
XLogRecPtr yb_amp_get_lwlsn(NRelFileInfo rlocator, ForkNumber forknum, BlockNumber blkno);
void yb_amp_get_lwlsn_v(NRelFileInfo relfilenode, ForkNumber forknum, BlockNumber blkno, int nblocks, XLogRecPtr *lsns);
XLogRecPtr yb_amp_set_lwlsn_block_range(XLogRecPtr lsn, NRelFileInfo rlocator, ForkNumber forknum, BlockNumber from, BlockNumber n_blocks);
XLogRecPtr yb_amp_set_lwlsn_block_v(const XLogRecPtr *lsns, NRelFileInfo relfilenode, ForkNumber forknum, BlockNumber blockno, int nblocks);
XLogRecPtr yb_amp_set_lwlsn_block(XLogRecPtr lsn, NRelFileInfo rlocator, ForkNumber forknum, BlockNumber blkno);
XLogRecPtr yb_amp_set_lwlsn_relation(XLogRecPtr lsn, NRelFileInfo rlocator, ForkNumber forknum);
XLogRecPtr yb_amp_set_lwlsn_db(XLogRecPtr lsn);

#endif /* YBAMP_LWLSNCACHE_H */