#ifndef CWR_SCHEDULER_H
#define CWR_SCHEDULER_H
#include "json_api.h"
J *cwr_scheduler_replace(J *);
J *cwr_scheduler_pick(J *);
void cwr_scheduler_observe(const char *,const char *,const char *,int,const char *,int);
void cwr_scheduler_usage(const char *,const char *,int,int);
void cwr_scheduler_clear(void);
#endif
