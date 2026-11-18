/* Named save states and deterministic reset for control protocol v2. Same GPL-3.0-or-later license as BlastEM. */
#ifndef CTRL_STATE_H_
#define CTRL_STATE_H_
#include <stddef.h>
#include <stdint.h>
#include "genesis.h"

enum { STATE_OK = 0, STATE_IO = -1, STATE_UNSUPPORTED = -2, STATE_NOT_FOUND = -3, STATE_MISMATCH = -4, STATE_CORRUPT = -5, STATE_INVALID = -6 };
int ctrl_state_valid_name(const char *name, size_t len);
/* Lowercase hex SHA-1 of the loaded ROM (41 bytes). */
void ctrl_state_rom_sha1(genesis_context *gen, char out[41]);
int ctrl_state_save(genesis_context *gen, const char *name, uint64_t *bytes);
int ctrl_state_load(genesis_context *gen, const char *name, uint32_t *saved_frame);
#endif
