/* Acknowledged control commands. Same GPL-3.0-or-later license as BlastEM. */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <inttypes.h>
#include "ctrl_v2.h"
#include "third_party/yyjson/yyjson.h"
#include "blastem.h"
#include "genesis.h"
#include "render.h"
#include "render_audio.h"
#include "vdp_trace.h"
#include "ctrl_state.h"
#include "gdb_remote.h"

#if !defined(USE_FBDEV) && !defined(IS_LIB) && !defined(DISABLE_ZLIB)
#define SNAPSHOT_CAPABILITY ",\"cached_screenshot\""
#else
#define SNAPSHOT_CAPABILITY ""
#endif

#ifndef NEW_CORE
#define Z80_CAPABILITY ",\"z80_observation\""
#else
#define Z80_CAPABILITY ""
#endif

#ifdef _WIN32
#define SRAM_CAPABILITY ""
#else
#define SRAM_CAPABILITY ",\"sram_flush\""
#endif

static uint64_t last_id;
static int negotiated;
static uint64_t pending_id;
static uint32_t frames_remaining, operation_start;
static int operation_interrupted, safe_stop, frame_stop;

/* Frame trace: sample watched values at each frame boundary of a bounded run. */
enum { SPACE_RAM, SPACE_CRAM, SPACE_VSRAM, SPACE_VREG, SPACE_VRAM, SPACE_Z80, SPACE_Z80_FRAME };

/* Z80 cycles of the last completed frame: executed, stopped by BUSREQ, held in reset. */
#ifndef NEW_CORE
#define Z80_STATS 1
static uint64_t z80_previous[3];
static uint32_t z80_frame_cycles[3];

static void z80_frame_update(genesis_context *gen)
{
	z80_context *z = gen->z80;
	uint64_t now[3] = {z->stat_run_mclks, z->stat_busack_mclks, z->stat_reset_mclks};
	for (int i = 0; i < 3; i++) {
		z80_frame_cycles[i] = (now[i] - z80_previous[i]) / z->options->gen.clock_divider;
		z80_previous[i] = now[i];
	}
}
#endif
#define TRACE_MAX_WATCHES 16
typedef struct { uint8_t space, size; uint32_t address; } trace_watch;
static trace_watch watches[TRACE_MAX_WATCHES];
static uint32_t watch_count, last_values[TRACE_MAX_WATCHES];
static FILE *trace_file;
static uint64_t trace_records;
static int trace_active, trace_failed, until_enabled, until_met, until_op, line_active;
static uint32_t until_watch, until_value, until_mask;

static uint32_t read_watch(genesis_context *gen, const trace_watch *w)
{
#ifdef Z80_STATS
	if (w->space == SPACE_Z80_FRAME) return z80_frame_cycles[w->address];
#endif
	uint32_t value = 0;
	for (uint32_t i = 0; i < w->size; i++) {
		uint32_t a = w->address + i;
		uint8_t byte = 0;
		switch (w->space)
		{
		case SPACE_RAM: { uint32_t offset = a & 0xFFFF; byte = gen->work_ram[offset >> 1] >> ((offset & 1) ? 0 : 8); break; }
		case SPACE_CRAM: byte = gen->vdp->cram[(a >> 1) & (CRAM_SIZE - 1)] >> ((a & 1) ? 0 : 8); break;
		case SPACE_VSRAM: byte = gen->vdp->vsram[(a >> 1) % MAX_VSRAM_SIZE] >> ((a & 1) ? 0 : 8); break;
		case SPACE_VREG: byte = gen->vdp->regs[a % VDP_REGS]; break;
		case SPACE_VRAM: byte = gen->vdp->vdpmem[a & 0xFFFF]; break;
		case SPACE_Z80: byte = gen->zram[a & 0x1FFF]; break;
		}
		value = value << 8 | byte;
	}
	return value;
}

static int parse_space(yyjson_val *v)
{
	static const char *names[] = {"ram", "cram", "vsram", "vdp_reg", "vram", "z80_ram", "z80_frame"};
	if (!v) return SPACE_RAM;
	for (int i = 0; i < 7; i++) if (yyjson_equals_str(v, names[i])) return i;
	return -1;
}

static int valid_watch(const trace_watch *w)
{
	uint32_t a = w->address, n = w->size;
	if (n != 1 && n != 2 && n != 4) return 0;
	switch (w->space)
	{
	case SPACE_RAM: return a >= 0xFF0000 && a + n <= 0x1000000;
	case SPACE_CRAM: return !(a & 1) && a + n <= CRAM_SIZE * 2;
	case SPACE_VSRAM: return !(a & 1) && a + n <= MAX_VSRAM_SIZE * 2;
	case SPACE_VREG: return n == 1 && a < VDP_REGS;
	case SPACE_VRAM: return a + n <= 0x10000;
	case SPACE_Z80: return a + n <= 0x2000;
#ifdef Z80_STATS
	case SPACE_Z80_FRAME: return a <= 2 && n == 4;
#endif
	}
	return 0;
}

static void trace_sample(genesis_context *gen, int first)
{
	uint32_t values[TRACE_MAX_WATCHES];
	int changed = first;
	for (uint32_t i = 0; i < watch_count; i++) {
		values[i] = read_watch(gen, watches + i);
		if (values[i] != last_values[i]) changed = 1;
	}
	if (until_enabled && !first) {
		uint32_t v = values[until_watch] & until_mask, previous = last_values[until_watch] & until_mask;
		if ((until_op == 0 && v == until_value) || (until_op == 1 && v != until_value) || (until_op == 2 && v != previous)) until_met = 1;
	}
	memcpy(last_values, values, sizeof(values));
	if (!changed) return;
	if (fprintf(trace_file, "{\"frame\":%u,\"values\":[", gen->vdp->frame) < 0) trace_failed = 1;
	for (uint32_t i = 0; i < watch_count; i++) {
		if (fprintf(trace_file, i ? ",%u" : "%u", values[i]) < 0) trace_failed = 1;
	}
	if (fputs("]}\n", trace_file) < 0) trace_failed = 1;
	trace_records++;
}

static int trace_close(void)
{
	if (!trace_file) return 0;
	int failed = trace_failed || fflush(trace_file) || ferror(trace_file);
	if (fclose(trace_file)) failed = 1;
	trace_file = NULL;
	trace_active = 0;
	return failed;
}

/* KITVDMP1 (see kit_prof.c), written immediately while stopped: little-endian header,
 * VRAM bytes, CRAM/VSRAM words and VDP registers. */
static int write_vdp_dump(vdp_context *vdp, const char *path)
{
	FILE *f = fopen(path, "wx");
	if (!f) return 0;
	uint8_t header[24] = "KITVDMP1";
	uint32_t counts[4] = {VRAM_SIZE, CRAM_SIZE, MAX_VSRAM_SIZE, VDP_REGS};
	for (int i = 0; i < 4; i++) for (int b = 0; b < 4; b++) header[8 + i * 4 + b] = counts[i] >> (8 * b);
	int ok = fwrite(header, 1, sizeof(header), f) == sizeof(header) && fwrite(vdp->vdpmem, 1, VRAM_SIZE, f) == VRAM_SIZE;
	for (int i = 0; ok && i < CRAM_SIZE; i++) { uint8_t w[2] = {vdp->cram[i], vdp->cram[i] >> 8}; ok = fwrite(w, 1, 2, f) == 2; }
	for (int i = 0; ok && i < MAX_VSRAM_SIZE; i++) { uint8_t w[2] = {vdp->vsram[i], vdp->vsram[i] >> 8}; ok = fwrite(w, 1, 2, f) == 2; }
	ok = ok && fwrite(vdp->regs, 1, VDP_REGS, f) == VDP_REGS;
	if (fclose(f)) ok = 0;
	if (!ok) remove(path);
	return ok;
}

void ctrl_v2_frame_boundary(void)
{
	if (current_system && current_system->debugger_type == DEBUGGER_GDB) gdb_remote_poll_interrupt(current_system);
#ifdef Z80_STATS
	if (current_system && current_system->type == SYSTEM_GENESIS) z80_frame_update((genesis_context *)current_system);
#endif
	if (!frames_remaining || !current_system) return;
	if (trace_active) {
		trace_sample((genesis_context *)current_system, 0);
		if (until_met) frames_remaining = 1;
	}
	if (--frames_remaining == 0) {
		current_system->paused = 1;
		frame_stop = 1;
		system_request_exit(current_system, 1);
	}
}

void ctrl_v2_safe_stop(int stopped) { safe_stop = stopped; if (!stopped) frame_stop = 0; }

int ctrl_v2_debug_stopped;
int ctrl_v2_disconnect;

void ctrl_v2_reset(void)
{
	/* A disconnected controller must not leave its bounded run executing. */
	if (pending_id) { frames_remaining = 1; pending_id = 0; }
	trace_close();
	if (line_active) { uint64_t ignored; vdp_trace_end(&ignored); line_active = 0; }
	ctrl_v2_disconnect = 0;
	last_id = 0;
	negotiated = 0;
}

static int valid_value(yyjson_val *v, int depth)
{
	if (depth > 16 || yyjson_is_real(v)) return 0;
	if (yyjson_is_str(v)) return yyjson_get_len(v) <= 4096;
	if (yyjson_is_obj(v)) {
		if (yyjson_obj_size(v) > 64) return 0;
		yyjson_obj_iter it = yyjson_obj_iter_with(v);
		yyjson_val *key;
		while ((key = yyjson_obj_iter_next(&it))) {
			if (!valid_value(key, depth + 1) || !valid_value(yyjson_obj_iter_get_val(key), depth + 1)) return 0;
			/* At most 64 fields: bounded duplicate-key validation. */
			yyjson_obj_iter other = yyjson_obj_iter_with(v);
			yyjson_val *k;
			while ((k = yyjson_obj_iter_next(&other)) && k != key) {
				if (yyjson_get_len(k) == yyjson_get_len(key) &&
				    !memcmp(yyjson_get_str(k), yyjson_get_str(key), yyjson_get_len(key))) return 0;
			}
		}
	} else if (yyjson_is_arr(v)) {
		size_t i, max;
		yyjson_val *item;
		yyjson_arr_foreach(v, i, max, item) {
			if (!valid_value(item, depth + 1)) return 0;
		}
	}
	return 1;
}

static char *reply(uint64_t id, const char *result, const char *error)
{
	char *out = malloc(2048);
	if (!out) return NULL;
	char identity[32];
	if (id) snprintf(identity, sizeof(identity), "%" PRIu64, id);
	else strcpy(identity, "null");
	if (error) {
		snprintf(out, 2048, "{\"id\":%s,\"version\":2,\"ok\":false,\"error\":{\"code\":\"%s\",\"message\":\"%s\"}}\n", identity, error, error);
	} else {
		snprintf(out, 2048, "{\"id\":%s,\"version\":2,\"ok\":true,\"result\":%s}\n", identity, result);
	}
	return out;
}

char *ctrl_v2_take_response(void)
{
	if (!pending_id || !safe_stop || !current_system || !current_system->paused) return NULL;
	genesis_context *gen = (genesis_context *)current_system;
	char result[384];
	int interrupted = operation_interrupted || (frames_remaining && !until_met);
	int n = snprintf(result, sizeof(result), "{\"frame\":%u,\"advanced\":%u,\"paused\":true,\"interrupted\":%s,\"boundary\":\"%s\"", gen->vdp->frame, gen->vdp->frame - operation_start, interrupted ? "true" : "false", frame_stop ? "frame_sync" : "paused_loop");
	const char *error = NULL;
	if (line_active) {
		uint64_t events;
		if (vdp_trace_end(&events)) error = "io_error";
		line_active = 0;
		snprintf(result + n, sizeof(result) - n, ",\"events\":%" PRIu64 "}", events);
	} else if (trace_active) {
		if (trace_close()) error = "io_error";
		snprintf(result + n, sizeof(result) - n, ",\"records\":%" PRIu64 ",\"stop\":\"%s\"}", trace_records, until_met ? "until" : interrupted ? "interrupted" : "frames");
	} else {
		snprintf(result + n, sizeof(result) - n, "}");
	}
	uint64_t id = pending_id;
	pending_id = 0;
	frames_remaining = 0;
	until_met = 0;
	return reply(id, result, error);
}

char *ctrl_v2_command(const char *line, size_t length)
{
	yyjson_doc *doc = yyjson_read(line, length, 0);
	yyjson_val *root = doc ? yyjson_doc_get_root(doc) : NULL;
	const char *error = "invalid_request";
	char result[1536];
	uint64_t id = 0;
	if (!yyjson_is_obj(root) || !valid_value(root, 0)) goto done;
	yyjson_val *identity = yyjson_obj_get(root, "id");
	if (!yyjson_is_uint(identity)) goto done;
	id = yyjson_get_uint(identity);
	if (!id || id > 9007199254740991ULL || id <= last_id) { id = 0; goto done; }
	last_id = id;
	yyjson_obj_iter iter = yyjson_obj_iter_with(root);
	yyjson_val *key;
	while ((key = yyjson_obj_iter_next(&iter))) {
		if (!yyjson_equals_str(key, "id") && !yyjson_equals_str(key, "version") &&
		    !yyjson_equals_str(key, "command") && !yyjson_equals_str(key, "params")) goto done;
	}
	yyjson_val *version = yyjson_obj_get(root, "version");
	if (!yyjson_is_uint(version)) goto done;
	if (yyjson_get_uint(version) != 2) { error = "unsupported_version"; ctrl_v2_disconnect = 1; goto done; }
	yyjson_val *command = yyjson_obj_get(root, "command");
	yyjson_val *params = yyjson_obj_get(root, "params");
	if (!yyjson_is_str(command) || !yyjson_is_obj(params)) goto done;
	if (yyjson_equals_str(command, "hello")) {
		yyjson_val *client = yyjson_obj_get(params, "client");
		if (yyjson_obj_size(params) != 1 || !yyjson_is_str(client)) { error = "invalid_params"; goto done; }
		negotiated = 1;
		strcpy(result, "{\"server\":\"blastem\",\"protocol\":2,\"limits\":{\"line_bytes\":65536,\"depth\":16,\"string_bytes\":4096,\"object_fields\":64},\"capabilities\":[\"save_directory\"" SRAM_CAPABILITY SNAPSHOT_CAPABILITY ",\"frame_control\",\"host_audio_capture\",\"speed_control\",\"paused_work_ram\",\"frame_trace\",\"paused_vdp_dump\",\"line_trace\",\"reset\",\"state\"" Z80_CAPABILITY "]}");
	} else {
		if (!negotiated) { error = "invalid_state"; goto done; }
		if (!yyjson_equals_str(command, "snapshot") && !yyjson_equals_str(command, "advance") && !yyjson_equals_str(command, "audio_start") && !yyjson_equals_str(command, "speed") && !yyjson_equals_str(command, "ram_read") && !yyjson_equals_str(command, "trace") && !yyjson_equals_str(command, "vdp_dump") && !yyjson_equals_str(command, "line_trace") && !yyjson_equals_str(command, "z80_read") && !yyjson_equals_str(command, "save_state") && !yyjson_equals_str(command, "load_state") && !yyjson_equals_str(command, "reset") && yyjson_obj_size(params)) { error = "invalid_params"; goto done; }
		if (!current_system || current_system->type != SYSTEM_GENESIS) { error = "unsupported"; goto done; }
		genesis_context *gen = (genesis_context *)current_system;
		if (pending_id && !yyjson_equals_str(command, "status") && !yyjson_equals_str(command, "interrupt")) { error = "busy"; goto done; }
		if (yyjson_equals_str(command, "trace")) {
			if (gen->header.debugger_type == DEBUGGER_GDB || gen->expansion || gen->mars || render_is_threaded_video()) { error = "unsupported"; goto done; }
			yyjson_val *frames = yyjson_obj_get(params, "frames"), *path = yyjson_obj_get(params, "path");
			yyjson_val *list = yyjson_obj_get(params, "watches"), *until = yyjson_obj_get(params, "until");
			const char *name = yyjson_get_str(path);
			size_t allowed = 3 + (until ? 1 : 0);
			if (yyjson_obj_size(params) != allowed || !yyjson_is_uint(frames) || !yyjson_get_uint(frames) || yyjson_get_uint(frames) > 36000 ||
			    !name || name[0] != '/' || strlen(name) != yyjson_get_len(path) || !yyjson_is_arr(list) ||
			    !yyjson_arr_size(list) || yyjson_arr_size(list) > TRACE_MAX_WATCHES) { error = "invalid_params"; goto done; }
			size_t i, max;
			yyjson_val *item;
			yyjson_arr_foreach(list, i, max, item) {
				yyjson_val *space = yyjson_obj_get(item, "space"), *address = yyjson_obj_get(item, "address"), *size = yyjson_obj_get(item, "size");
				size_t fields = 1 + (space ? 1 : 0) + (size ? 1 : 0);
				int kind = parse_space(space);
				if (!yyjson_is_obj(item) || yyjson_obj_size(item) != fields || kind < 0 || !yyjson_is_uint(address) || (size && !yyjson_is_uint(size)) ||
				    yyjson_get_uint(address) > 0xFFFFFF || (size && yyjson_get_uint(size) > 4)) { error = "invalid_params"; goto done; }
				watches[i] = (trace_watch){.space = kind, .size = size ? yyjson_get_uint(size) : 1, .address = yyjson_get_uint(address)};
				if (!valid_watch(watches + i)) { error = "invalid_params"; goto done; }
			}
			watch_count = max;
			until_enabled = 0;
			if (until) {
				yyjson_val *watch = yyjson_obj_get(until, "watch"), *op = yyjson_obj_get(until, "op"), *value = yyjson_obj_get(until, "value"), *mask = yyjson_obj_get(until, "mask");
				size_t fields = 2 + (value ? 1 : 0) + (mask ? 1 : 0);
				int kind = !op ? -1 : yyjson_equals_str(op, "eq") ? 0 : yyjson_equals_str(op, "ne") ? 1 : yyjson_equals_str(op, "changed") ? 2 : -1;
				if (!yyjson_is_obj(until) || yyjson_obj_size(until) != fields || kind < 0 || !yyjson_is_uint(watch) || yyjson_get_uint(watch) >= watch_count ||
				    (kind != 2 && !yyjson_is_uint(value)) || (kind == 2 && value) || (value && yyjson_get_uint(value) > 0xFFFFFFFF) ||
				    (mask && (!yyjson_is_uint(mask) || yyjson_get_uint(mask) > 0xFFFFFFFF))) { error = "invalid_params"; goto done; }
				until_enabled = 1; until_op = kind; until_watch = yyjson_get_uint(watch);
				until_mask = mask ? yyjson_get_uint(mask) : 0xFFFFFFFF;
				until_value = value ? (yyjson_get_uint(value) & until_mask) : 0;
			}
			if (!safe_stop || !gen->header.paused) { error = "invalid_state"; goto done; }
			trace_file = fopen(name, "wx");
			if (!trace_file) { error = "io_error"; goto done; }
			trace_active = 1; trace_failed = 0; trace_records = 0; until_met = 0;
			trace_sample(gen, 1);
			pending_id = id;
			frames_remaining = yyjson_get_uint(frames);
			operation_start = gen->vdp->frame;
			operation_interrupted = 0;
			gen->header.paused = 0;
			yyjson_doc_free(doc);
			return NULL; /* completion follows at the main loop's safe stop */
		} else if (yyjson_equals_str(command, "line_trace")) {
			if (gen->header.debugger_type == DEBUGGER_GDB || gen->expansion || gen->mars || render_is_threaded_video()) { error = "unsupported"; goto done; }
			yyjson_val *frames = yyjson_obj_get(params, "frames"), *path = yyjson_obj_get(params, "path"), *kinds = yyjson_obj_get(params, "kinds");
			const char *name = yyjson_get_str(path);
			if (yyjson_obj_size(params) != 2 + (kinds ? 1 : 0) || !yyjson_is_uint(frames) || !yyjson_get_uint(frames) || yyjson_get_uint(frames) > 600 ||
			    !name || name[0] != '/' || strlen(name) != yyjson_get_len(path) || (kinds && (!yyjson_is_arr(kinds) || !yyjson_arr_size(kinds)))) { error = "invalid_params"; goto done; }
			uint32_t mask = kinds ? 0 : VDP_TRACE_CRAM | VDP_TRACE_VSRAM | VDP_TRACE_REG | VDP_TRACE_INT;
			if (kinds) {
				size_t i, max;
				yyjson_val *item;
				yyjson_arr_foreach(kinds, i, max, item) {
					uint32_t bit = yyjson_equals_str(item, "cram") ? VDP_TRACE_CRAM : yyjson_equals_str(item, "vsram") ? VDP_TRACE_VSRAM :
					               yyjson_equals_str(item, "reg") ? VDP_TRACE_REG : yyjson_equals_str(item, "int") ? VDP_TRACE_INT : 0;
					if (!bit) { error = "invalid_params"; goto done; }
					mask |= bit;
				}
			}
			if (!safe_stop || !gen->header.paused) { error = "invalid_state"; goto done; }
			if (!vdp_trace_begin(name, mask)) { error = "io_error"; goto done; }
			line_active = 1;
			pending_id = id;
			frames_remaining = yyjson_get_uint(frames);
			operation_start = gen->vdp->frame;
			operation_interrupted = 0;
			gen->header.paused = 0;
			yyjson_doc_free(doc);
			return NULL;
#ifdef Z80_STATS
		} else if (yyjson_equals_str(command, "z80_read")) {
			yyjson_val *address = yyjson_obj_get(params, "address"), *size = yyjson_obj_get(params, "size");
			if (yyjson_obj_size(params) != 2 || !yyjson_is_uint(address) || !yyjson_is_uint(size)) { error = "invalid_params"; goto done; }
			uint64_t addr = yyjson_get_uint(address), count = yyjson_get_uint(size);
			if (addr > 0x1FFF || !count || count > 256 || addr + count > 0x2000) { error = "invalid_params"; goto done; }
			if (!(gen->header.paused && safe_stop) && !ctrl_v2_debug_stopped) { error = "invalid_state"; goto done; }
			char hex[513];
			static const char digits[] = "0123456789abcdef";
			for (uint32_t i = 0; i < count; i++) {
				uint8_t byte = gen->zram[addr + i];
				hex[i*2] = digits[byte >> 4]; hex[i*2+1] = digits[byte & 15];
			}
			hex[count*2] = 0;
			snprintf(result, sizeof(result), "{\"address\":%u,\"size\":%u,\"frame\":%u,\"hex\":\"%s\"}", (uint32_t)addr, (uint32_t)count, gen->vdp->frame, hex);
		} else if (yyjson_equals_str(command, "z80_state")) {
			if (!(gen->header.paused && safe_stop) && !ctrl_v2_debug_stopped) { error = "invalid_state"; goto done; }
			/* The JIT does not keep the Z80 PC current between sync points, so it is not reported. */
			z80_context *z = gen->z80;
			uint8_t *f = z->flags;
			uint8_t flags = f[ZF_S] << 7 | f[ZF_Z] << 6 | f[ZF_H] << 4 | f[ZF_PV] << 2 | f[ZF_N] << 1 | f[ZF_C];
			uint32_t divider = z->options->gen.clock_divider;
			snprintf(result, sizeof(result),
				"{\"frame\":%u,\"sp\":%u,\"a\":%u,\"f\":%u,\"bc\":%u,\"de\":%u,\"hl\":%u,\"ix\":%u,\"iy\":%u,\"i\":%u,\"im\":%u,\"iff1\":%u,\"iff2\":%u,"
				"\"bank\":%u,\"reset\":%s,\"busreq\":%s,\"busack\":%s,\"last_frame\":{\"run\":%u,\"busack\":%u,\"reset\":%u},"
				"\"total\":{\"run\":%" PRIu64 ",\"busack\":%" PRIu64 ",\"reset\":%" PRIu64 "}}",
				gen->vdp->frame, z->sp, (uint8_t)z->regs[Z80_A], flags, (uint8_t)z->regs[Z80_B] << 8 | (uint8_t)z->regs[Z80_C],
				(uint8_t)z->regs[Z80_D] << 8 | (uint8_t)z->regs[Z80_E], (uint8_t)z->regs[Z80_H] << 8 | (uint8_t)z->regs[Z80_L],
				(uint8_t)z->regs[Z80_IXH] << 8 | (uint8_t)z->regs[Z80_IXL], (uint8_t)z->regs[Z80_IYH] << 8 | (uint8_t)z->regs[Z80_IYL],
				(uint8_t)z->regs[Z80_I], z->im, z->iff1, z->iff2, z->bank_reg,
				z->reset ? "true" : "false", z->busreq ? "true" : "false", z->busack ? "true" : "false",
				z80_frame_cycles[0], z80_frame_cycles[1], z80_frame_cycles[2],
				z->stat_run_mclks / divider, z->stat_busack_mclks / divider, z->stat_reset_mclks / divider);
#endif
		} else if (yyjson_equals_str(command, "save_state") || yyjson_equals_str(command, "load_state")) {
			yyjson_val *name = yyjson_obj_get(params, "name");
			const char *text = yyjson_get_str(name);
			if (yyjson_obj_size(params) != 1 || !text || !ctrl_state_valid_name(text, yyjson_get_len(name))) { error = "invalid_params"; goto done; }
			if (gen->header.debugger_type == DEBUGGER_GDB || render_is_threaded_video()) { error = "unsupported"; goto done; }
			if (!gen->header.paused || !safe_stop) { error = "invalid_state"; goto done; }
			int status;
			if (yyjson_equals_str(command, "save_state")) {
				uint64_t bytes = 0;
				status = ctrl_state_save(gen, text, &bytes);
				snprintf(result, sizeof(result), "{\"name\":\"%s\",\"bytes\":%" PRIu64 ",\"frame\":%u,\"paused\":true}", text, bytes, gen->vdp->frame);
			} else {
				uint32_t saved_frame = 0;
				status = ctrl_state_load(gen, text, &saved_frame);
				snprintf(result, sizeof(result), "{\"name\":\"%s\",\"frame\":%u,\"saved_frame\":%u,\"paused\":true}", text, gen->vdp->frame, saved_frame);
			}
			if (status) {
				error = status == STATE_UNSUPPORTED ? "unsupported" : status == STATE_NOT_FOUND ? "not_found" :
				        status == STATE_MISMATCH ? "invalid_state" : "io_error";
				goto done;
			}
		} else if (yyjson_equals_str(command, "reset")) {
			yyjson_val *kind = yyjson_obj_get(params, "kind");
			if (yyjson_obj_size(params) > 1 || (yyjson_obj_size(params) == 1 && !kind) || (kind && !yyjson_equals_str(kind, "soft"))) { error = kind && yyjson_equals_str(kind, "hard") ? "unsupported" : "invalid_params"; goto done; }
			if (gen->header.debugger_type == DEBUGGER_GDB || gen->expansion || gen->mars) { error = "unsupported"; goto done; }
			if (!gen->header.paused || !safe_stop) { error = "invalid_state"; goto done; }
			/* Deterministic: the reset line is asserted at the first cycle after resuming,
			 * instead of the random delay used by the interactive reset button. */
			gen->reset_cycle = gen->m68k->cycles;
			snprintf(result, sizeof(result), "{\"kind\":\"soft\",\"scheduled\":true,\"frame\":%u,\"paused\":true}", gen->vdp->frame);
		} else if (yyjson_equals_str(command, "vdp_dump")) {
			yyjson_val *path = yyjson_obj_get(params, "path");
			const char *name = yyjson_get_str(path);
			if (yyjson_obj_size(params) != 1 || !name || name[0] != '/' || strlen(name) != yyjson_get_len(path)) { error = "invalid_params"; goto done; }
			if (!(gen->header.paused && safe_stop) && !ctrl_v2_debug_stopped) { error = "invalid_state"; goto done; }
			if (!write_vdp_dump(gen->vdp, name)) { error = "io_error"; goto done; }
			snprintf(result, sizeof(result), "{\"frame\":%u,\"paused\":true,\"format\":\"KITVDMP1\"}", gen->vdp->frame);
		} else if (yyjson_equals_str(command, "ram_read")) {
            yyjson_val *address = yyjson_obj_get(params, "address"), *size = yyjson_obj_get(params, "size");
            if (yyjson_obj_size(params) != 2 || !yyjson_is_uint(address) || !yyjson_is_uint(size)) { error = "invalid_params"; goto done; }
            uint64_t addr = yyjson_get_uint(address), count = yyjson_get_uint(size);
            if (addr < 0xFF0000 || addr > 0xFFFFFF || !count || count > 256 || count > 0x1000000 - addr) { error = "invalid_params"; goto done; }
            if ((!gen->header.paused || !safe_stop) && !ctrl_v2_debug_stopped) { error = "invalid_state"; goto done; }
            char hex[513];
            static const char digits[] = "0123456789abcdef";
            for (uint32_t i = 0; i < count; i++) {
                uint32_t offset = (addr + i) & 0xFFFF;
                uint8_t byte = gen->work_ram[offset >> 1] >> ((offset & 1) ? 0 : 8);
                hex[i*2] = digits[byte >> 4]; hex[i*2+1] = digits[byte & 15];
            }
            hex[count*2] = 0;
            snprintf(result, sizeof(result), "{\"address\":%u,\"size\":%u,\"frame\":%u,\"hex\":\"%s\"}", (uint32_t)addr, (uint32_t)count, gen->vdp->frame, hex);
        } else if (yyjson_equals_str(command, "speed")) {
            yyjson_val *percent = yyjson_obj_get(params, "percent");
            if (yyjson_obj_size(params) != 1 || !yyjson_is_uint(percent) || yyjson_get_uint(percent) < 25 || yyjson_get_uint(percent) > 1000) { error = "invalid_params"; goto done; }
            if (gen->header.debugger_type == DEBUGGER_GDB || gen->expansion || gen->mars || render_is_threaded_video() || !current_system->set_speed_percent) { error = "unsupported"; goto done; }
            if (!gen->header.paused || !safe_stop) { error = "invalid_state"; goto done; }
            uint32_t speed = yyjson_get_uint(percent);
            current_system->set_speed_percent(current_system, speed);
            snprintf(result, sizeof(result), "{\"percent\":%u,\"frame\":%u,\"paused\":true}", speed, gen->vdp->frame);
        } else if (yyjson_equals_str(command, "audio_start")) {
			yyjson_val *path = yyjson_obj_get(params, "path"), *seconds = yyjson_obj_get(params, "max_seconds");
			const char *name = yyjson_get_str(path);
			if (yyjson_obj_size(params) != 2 || !name || name[0] != '/' || strlen(name) != yyjson_get_len(path) || !yyjson_is_uint(seconds) || !yyjson_get_uint(seconds) || yyjson_get_uint(seconds) > 30) { error = "invalid_params"; goto done; }
			uint32_t rate;
			int status = render_capture_begin(name, yyjson_get_uint(seconds), &rate);
			if (status) { error = status == -2 ? "unsupported" : status == -3 ? "busy" : "io_error"; goto done; }
			snprintf(result, sizeof(result), "{\"sample_rate\":%u,\"channels\":2,\"boundary\":\"host_mixer\"}", rate);
		} else if (yyjson_equals_str(command, "audio_stop")) {
			uint64_t samples;
			uint32_t rate;
			uint8_t limited;
			int status = render_capture_end(&samples, &rate, &limited);
			if (status) { error = status == -3 ? "invalid_state" : "io_error"; goto done; }
			snprintf(result, sizeof(result), "{\"sample_rate\":%u,\"channels\":2,\"samples\":%" PRIu64 ",\"limited\":%s,\"boundary\":\"host_mixer\"}", rate, samples, limited ? "true" : "false");
		} else if (yyjson_equals_str(command, "pause") || yyjson_equals_str(command, "advance") || yyjson_equals_str(command, "interrupt")) {
			/* GDB owns resume/stop replies in debug sessions. Keep a single owner. */
			if (gen->header.debugger_type == DEBUGGER_GDB || gen->expansion || gen->mars || render_is_threaded_video()) { error = "unsupported"; goto done; }
			if (yyjson_equals_str(command, "interrupt")) {
				if (!pending_id) { error = "invalid_state"; goto done; }
				frames_remaining = 1;
				operation_interrupted = 1;
				strcpy(result, "{\"requested\":true}");
			} else {
				uint32_t count = 1;
				if (yyjson_equals_str(command, "advance")) {
					yyjson_val *frames = yyjson_obj_get(params, "frames");
					if (yyjson_obj_size(params) != 1 || !yyjson_is_uint(frames) || !yyjson_get_uint(frames) || yyjson_get_uint(frames) > 600) { error = "invalid_params"; goto done; }
					if (!safe_stop || !gen->header.paused) { error = "invalid_state"; goto done; }
					count = yyjson_get_uint(frames);
				} else if (safe_stop && gen->header.paused && frame_stop) {
					snprintf(result, sizeof(result), "{\"frame\":%u,\"advanced\":0,\"paused\":true,\"interrupted\":false,\"boundary\":\"frame_sync\"}", gen->vdp->frame);
					error = NULL; goto done;
				}
				pending_id = id;
				frames_remaining = count;
				operation_start = gen->vdp->frame;
				operation_interrupted = 0;
				gen->header.paused = 0;
				yyjson_doc_free(doc);
				return NULL; /* completion follows at the main loop's safe stop */
			}
		} else if (yyjson_equals_str(command, "snapshot")) {
#if !defined(USE_FBDEV) && !defined(IS_LIB) && !defined(DISABLE_ZLIB)
			yyjson_val *path = yyjson_obj_get(params, "path");
			const char *name = yyjson_get_str(path);
			if (yyjson_obj_size(params) != 1 || !name || name[0] != '/' || strlen(name) != yyjson_get_len(path)) { error = "invalid_params"; goto done; }
			if (!gen->header.paused && !ctrl_v2_debug_stopped) { error = "invalid_state"; goto done; }
			uint32_t width, height;
			uint64_t sequence;
			int status = render_snapshot(name, &width, &height, &sequence);
			if (status) { error = status == -2 ? "unsupported" : status == -3 ? "not_ready" : "io_error"; goto done; }
			snprintf(result, sizeof(result), "{\"width\":%u,\"height\":%u,\"sequence\":%" PRIu64 ",\"frame\":%u,\"image_boundary\":\"last_completed_progressive_frame\",\"paused\":true}", width, height, sequence, gen->vdp->frame);
#else
			error = "unsupported"; goto done;
#endif
		} else if (yyjson_equals_str(command, "save_flush")) {
			int status = genesis_flush_sram(gen);
			if (status) { error = status == -2 ? "unsupported" : "io_error"; goto done; }
			snprintf(result, sizeof(result), "{\"bytes\":%u,\"frame\":%u,\"paused\":%s}", gen->save_size, gen->vdp->frame, (gen->header.paused || ctrl_v2_debug_stopped) ? "true" : "false");
		} else if (yyjson_equals_str(command, "status")) {
			char rom_sha1[41];
			ctrl_state_rom_sha1(gen, rom_sha1);
			snprintf(result, sizeof(result), "{\"system\":\"genesis\",\"rom_sha1\":\"%s\",\"frame\":%u,\"paused\":%s,\"boundary\":\"%s\"}", rom_sha1, gen->vdp->frame, (gen->header.paused || ctrl_v2_debug_stopped) ? "true" : "false", ctrl_v2_debug_stopped ? "instruction" : (safe_stop && gen->header.paused) ? (frame_stop ? "frame_sync" : "paused_loop") : "event_poll");
		} else { error = "unknown_command"; goto done; }
	}
	error = NULL;
done:
	yyjson_doc_free(doc);
	return reply(id, result, error);
}
