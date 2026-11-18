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

#if !defined(USE_FBDEV) && !defined(IS_LIB) && !defined(DISABLE_ZLIB)
#define SNAPSHOT_CAPABILITY ",\"cached_screenshot\""
#else
#define SNAPSHOT_CAPABILITY ""
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

void ctrl_v2_frame_boundary(void)
{
	if (!frames_remaining || !current_system) return;
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
	char result[256];
	snprintf(result, sizeof(result), "{\"frame\":%u,\"advanced\":%u,\"paused\":true,\"interrupted\":%s,\"boundary\":\"%s\"}", gen->vdp->frame, gen->vdp->frame - operation_start, (operation_interrupted || frames_remaining) ? "true" : "false", frame_stop ? "frame_sync" : "paused_loop");
	uint64_t id = pending_id;
	pending_id = 0;
	frames_remaining = 0;
	return reply(id, result, NULL);
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
		strcpy(result, "{\"server\":\"blastem\",\"protocol\":2,\"limits\":{\"line_bytes\":65536,\"depth\":16,\"string_bytes\":4096,\"object_fields\":64},\"capabilities\":[\"save_directory\"" SRAM_CAPABILITY SNAPSHOT_CAPABILITY ",\"frame_control\",\"host_audio_capture\",\"speed_control\",\"paused_work_ram\"" "]}");
	} else {
		if (!negotiated) { error = "invalid_state"; goto done; }
		if (!yyjson_equals_str(command, "snapshot") && !yyjson_equals_str(command, "advance") && !yyjson_equals_str(command, "audio_start") && !yyjson_equals_str(command, "speed") && !yyjson_equals_str(command, "ram_read") && yyjson_obj_size(params)) { error = "invalid_params"; goto done; }
		if (!current_system || current_system->type != SYSTEM_GENESIS) { error = "unsupported"; goto done; }
		genesis_context *gen = (genesis_context *)current_system;
		if (pending_id && !yyjson_equals_str(command, "status") && !yyjson_equals_str(command, "interrupt")) { error = "busy"; goto done; }
		if (yyjson_equals_str(command, "ram_read")) {
            yyjson_val *address = yyjson_obj_get(params, "address"), *size = yyjson_obj_get(params, "size");
            if (yyjson_obj_size(params) != 2 || !yyjson_is_uint(address) || !yyjson_is_uint(size)) { error = "invalid_params"; goto done; }
            uint64_t addr = yyjson_get_uint(address), count = yyjson_get_uint(size);
            if (addr < 0xFF0000 || addr > 0xFFFFFF || !count || count > 256 || count > 0x1000000 - addr) { error = "invalid_params"; goto done; }
            if (!gen->header.paused || !safe_stop) { error = "invalid_state"; goto done; }
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
			snprintf(result, sizeof(result), "{\"system\":\"genesis\",\"frame\":%u,\"paused\":%s,\"boundary\":\"%s\"}", gen->vdp->frame, (gen->header.paused || ctrl_v2_debug_stopped) ? "true" : "false", ctrl_v2_debug_stopped ? "instruction" : (safe_stop && gen->header.paused) ? (frame_stop ? "frame_sync" : "paused_loop") : "event_poll");
		} else { error = "unknown_command"; goto done; }
	}
	error = NULL;
done:
	yyjson_doc_free(doc);
	return reply(id, result, error);
}
