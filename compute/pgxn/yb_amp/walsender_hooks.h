#ifndef __WALSENDER_HOOKS_H__
#define __WALSENDER_HOOKS_H__

struct XLogReaderRoutine;
void		YbAmpOnDemandXLogReaderRoutines(struct XLogReaderRoutine *xlr);

#endif
