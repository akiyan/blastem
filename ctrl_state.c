/* Named save states and deterministic reset for control protocol v2. Same GPL-3.0-or-later license as BlastEM. */
#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#ifndef _WIN32
#include <fcntl.h>
#include <unistd.h>
#endif
#include "ctrl_state.h"
#include "hash.h"

#define STATE_MAGIC "AKSTATE1"
/* magic(8) version(4) rom_sha1(20) frame(4) payload_size(8) payload_sha1(20) */
#define STATE_HEADER 64

static void put32(uint8_t *p, uint32_t v) { for (int i = 0; i < 4; i++) p[i] = v >> (8 * i); }
static void put64(uint8_t *p, uint64_t v) { for (int i = 0; i < 8; i++) p[i] = v >> (8 * i); }
static uint32_t get32(const uint8_t *p) { uint32_t v = 0; for (int i = 3; i >= 0; i--) v = v << 8 | p[i]; return v; }
static uint64_t get64(const uint8_t *p) { uint64_t v = 0; for (int i = 7; i >= 0; i--) v = v << 8 | p[i]; return v; }

int ctrl_state_valid_name(const char *name, size_t len)
{
	if (!len || len > 64 || strlen(name) != len || !strcmp(name, ".") || !strcmp(name, "..")) return 0;
	for (size_t i = 0; i < len; i++) {
		char c = name[i];
		int alnum = (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9');
		if (!alnum && (!i || (c != '.' && c != '_' && c != '-'))) return 0;
	}
	return 1;
}

static void rom_digest(genesis_context *gen, uint8_t out[20])
{
	static void *cached_rom;
	static uint32_t cached_size;
	static uint8_t digest[20];
	if (cached_rom != gen->header.info.rom || cached_size != gen->header.info.rom_size) {
		sha1(gen->header.info.rom, gen->header.info.rom_size, digest);
		cached_rom = gen->header.info.rom;
		cached_size = gen->header.info.rom_size;
	}
	memcpy(out, digest, 20);
}

void ctrl_state_rom_sha1(genesis_context *gen, char out[41])
{
	uint8_t digest[20];
	rom_digest(gen, digest);
	for (int i = 0; i < 20; i++) sprintf(out + 2 * i, "%02x", digest[i]);
}

static char *state_path(const char *name, const char *suffix)
{
	const char *dir = getenv("BLASTEM_STATE_DIR");
	if (!dir || dir[0] != '/') return NULL;
	size_t size = strlen(dir) + strlen(name) + strlen(suffix) + 3;
	char *path = malloc(size);
	if (path) snprintf(path, size, "%s/%s%s", dir, name, suffix);
	return path;
}

int ctrl_state_save(genesis_context *gen, const char *name, uint64_t *bytes)
{
#ifdef _WIN32
	return STATE_UNSUPPORTED;
#else
	if (!gen->header.serialize || gen->expansion || gen->mars) return STATE_UNSUPPORTED;
	char *path = state_path(name, ".state"), *tmp = state_path(name, ".state.tmp.XXXXXX");
	if (!path || !tmp) { free(path); free(tmp); return STATE_UNSUPPORTED; }
	size_t size = 0;
	uint8_t *payload = gen->header.serialize(&gen->header, &size);
	if (!payload || !size) { free(path); free(tmp); free(payload); return STATE_IO; }
	uint8_t header[STATE_HEADER] = STATE_MAGIC;
	put32(header + 8, 1);
	rom_digest(gen, header + 12);
	put32(header + 32, gen->vdp->frame);
	put64(header + 36, size);
	sha1(payload, size, header + 44);
	int failed = 0, fd = mkstemp(tmp);
	if (fd < 0) failed = 1;
	for (int part = 0; !failed && part < 2; part++) {
		uint8_t *data = part ? payload : header;
		size_t length = part ? size : sizeof(header), offset = 0;
		while (offset < length) {
			ssize_t n = write(fd, data + offset, length - offset);
			if (n <= 0) { failed = 1; break; }
			offset += n;
		}
	}
	if (fd >= 0) {
		if (fsync(fd)) failed = 1;
		if (close(fd)) failed = 1;
		if (!failed && rename(tmp, path)) failed = 1;
		if (failed) unlink(tmp);
	}
	if (!failed) {
		char *slash = strrchr(path, '/');
		*slash = 0;
		int directory = open(path, O_RDONLY | O_DIRECTORY);
		if (directory < 0 || fsync(directory)) failed = 1;
		if (directory >= 0) close(directory);
	}
	free(payload);
	free(path);
	free(tmp);
	*bytes = size;
	return failed ? STATE_IO : STATE_OK;
#endif
}

int ctrl_state_load(genesis_context *gen, const char *name, uint32_t *saved_frame)
{
	if (!gen->header.deserialize || gen->expansion || gen->mars) return STATE_UNSUPPORTED;
	char *path = state_path(name, ".state");
	if (!path) return STATE_UNSUPPORTED;
	FILE *f = fopen(path, "rb");
	free(path);
	if (!f) return errno == ENOENT ? STATE_NOT_FOUND : STATE_IO;
	uint8_t header[STATE_HEADER], digest[20], rom[20];
	if (fread(header, 1, sizeof(header), f) != sizeof(header) || memcmp(header, STATE_MAGIC, 8) || get32(header + 8) != 1) {
		fclose(f);
		return STATE_CORRUPT;
	}
	rom_digest(gen, rom);
	if (memcmp(header + 12, rom, 20)) { fclose(f); return STATE_MISMATCH; }
	uint64_t size = get64(header + 36);
	if (!size || size > 64 * 1024 * 1024) { fclose(f); return STATE_CORRUPT; }
	uint8_t *payload = malloc(size);
	int extra = payload && fread(payload, 1, size, f) == size ? fgetc(f) : 0;
	fclose(f);
	if (!payload || extra != EOF) { free(payload); return STATE_CORRUPT; }
	sha1(payload, size, digest);
	if (memcmp(digest, header + 44, 20)) { free(payload); return STATE_CORRUPT; }
	gen->header.deserialize(&gen->header, payload, size);
	free(payload);
	/* The frame-change detector is not part of the state; without this the first
	 * sync after loading would count a spurious frame boundary. */
	gen->last_frame = gen->vdp->frame;
	*saved_frame = get32(header + 32);
	return STATE_OK;
}
