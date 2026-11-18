/* Scanline-stamped VDP write and interrupt log. Same GPL-3.0-or-later license as BlastEM. */
#include "vdp_trace.h"
#include "vdp.h"

FILE *vdp_trace_file;
uint32_t vdp_trace_kinds;
static uint64_t events;
static int failed;
/* Acknowledged interrupts awaiting RTE (68000 cycles are master clocks / 7). */
static struct { uint32_t level, cycle; } stack[8];
static int depth;

int vdp_trace_begin(const char *path, uint32_t kinds)
{
	vdp_trace_file = fopen(path, "wx");
	if (!vdp_trace_file) return 0;
	vdp_trace_kinds = kinds;
	events = 0;
	failed = 0;
	depth = 0;
	return 1;
}

int vdp_trace_end(uint64_t *count)
{
	*count = events;
	if (!vdp_trace_file) return 0;
	int bad = failed || fflush(vdp_trace_file) || ferror(vdp_trace_file);
	bad |= fclose(vdp_trace_file) != 0;
	vdp_trace_file = NULL;
	vdp_trace_kinds = 0;
	return bad;
}

static void emit(struct vdp_context *vdp, const char *kind, const char *fields)
{
	if (fprintf(vdp_trace_file, "{\"frame\":%u,\"line\":%u,\"slot\":%u,\"kind\":\"%s\",%s}\n",
	            vdp->frame, vdp->vcounter, vdp->hslot, kind, fields) < 0) failed = 1;
	events++;
}

void vdp_trace_write(struct vdp_context *vdp, uint32_t kind, uint32_t address, uint32_t value)
{
	if (!vdp_trace_file || !(vdp_trace_kinds & kind)) return;
	char fields[64];
	snprintf(fields, sizeof(fields), kind == VDP_TRACE_REG ? "\"reg\":%u,\"value\":%u" : "\"address\":%u,\"value\":%u", address, value);
	emit(vdp, kind == VDP_TRACE_CRAM ? "cram" : kind == VDP_TRACE_VSRAM ? "vsram" : "reg", fields);
}

void vdp_trace_int(struct vdp_context *vdp, uint32_t level, uint32_t cycle)
{
	if (!vdp_trace_file || !(vdp_trace_kinds & VDP_TRACE_INT)) return;
	if (depth < 8) { stack[depth].level = level; stack[depth].cycle = cycle; }
	depth++;
	char fields[32];
	snprintf(fields, sizeof(fields), "\"level\":%u", level);
	emit(vdp, "int", fields);
}

void vdp_trace_rte(struct vdp_context *vdp, uint32_t cycle)
{
	if (!vdp_trace_file || !(vdp_trace_kinds & VDP_TRACE_INT)) return;
	char fields[64];
	if (depth > 0 && depth <= 8) {
		depth--;
		snprintf(fields, sizeof(fields), "\"level\":%u,\"cycles\":%u", stack[depth].level, (cycle - stack[depth].cycle) / 7);
	} else {
		if (depth > 8) depth--;
		snprintf(fields, sizeof(fields), "\"level\":null");
	}
	emit(vdp, "rte", fields);
}

void vdp_trace_adjust(uint32_t deduction)
{
	for (int i = 0; i < depth && i < 8; i++) stack[i].cycle -= deduction;
}
