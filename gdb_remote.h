#ifndef GDB_REMOTE_H_
#define GDB_REMOTE_H_
#include "genesis.h"

void gdb_remote_init(void);
#include "system.h"
void gdb_remote_poll_interrupt(system_header *system);
void gdb_debug_enter(void *vcontext, uint32_t pc);

#endif //GDB_REMOTE_H_
