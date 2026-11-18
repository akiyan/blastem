/* Scanline-stamped VDP write and interrupt log. Same GPL-3.0-or-later license as BlastEM. */
#ifndef VDP_TRACE_H_
#define VDP_TRACE_H_
#include <stdint.h>
#include <stdio.h>

struct vdp_context;
enum { VDP_TRACE_CRAM = 1, VDP_TRACE_VSRAM = 2, VDP_TRACE_REG = 4, VDP_TRACE_INT = 8 };
extern FILE *vdp_trace_file;
extern uint32_t vdp_trace_kinds;

int vdp_trace_begin(const char *path, uint32_t kinds);
/* Returns nonzero on write failure; stores the event count. */
int vdp_trace_end(uint64_t *events);
void vdp_trace_write(struct vdp_context *vdp, uint32_t kind, uint32_t address, uint32_t value);
void vdp_trace_int(struct vdp_context *vdp, uint32_t level, uint32_t cycle);
void vdp_trace_rte(struct vdp_context *vdp, uint32_t cycle);
void vdp_trace_adjust(uint32_t deduction);
#endif
