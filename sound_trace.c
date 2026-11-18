/* Per-channel muting and register-write logging for sound analysis. Same GPL-3.0-or-later license as BlastEM. */
#include <string.h>
#include "sound_trace.h"

uint16_t sound_mute_mask;
FILE *sound_log;
static uint32_t log_frame = 1, dac_writes; /* 1-based frame being emulated */
static int log_failed;
static const char *names[SOUND_CHANNELS] = {"fm1", "fm2", "fm3", "fm4", "fm5", "fm6", "dac", "psg1", "psg2", "psg3", "noise"};

int sound_parse_channels(const char *list, uint16_t *mask)
{
	*mask = 0;
	while (*list) {
		const char *end = strchr(list, ',');
		size_t len = end ? (size_t)(end - list) : strlen(list);
		int found = 0;
		if (len == 2 && !strncmp(list, "fm", 2)) { *mask |= 0x3F; found = 1; }
		else if (len == 3 && !strncmp(list, "psg", 3)) { *mask |= 0xF << SOUND_PSG1; found = 1; }
		for (int i = 0; !found && i < SOUND_CHANNELS; i++) {
			if (strlen(names[i]) == len && !strncmp(list, names[i], len)) { *mask |= 1 << i; found = 1; }
		}
		if (!found) return 0;
		if (!end) break;
		list = end + 1;
	}
	return *mask != 0;
}

void sound_mute_names(char *out, size_t size)
{
	size_t used = 0;
	out[0] = 0;
	for (int i = 0; i < SOUND_CHANNELS; i++) {
		if (sound_mute_mask & (1 << i)) {
			int n = snprintf(out + used, size - used, "%s\"%s\"", used ? "," : "", names[i]);
			if (n < 0 || (size_t)n >= size - used) break;
			used += n;
		}
	}
}

void sound_trace_ym(uint8_t part, uint8_t reg, uint8_t value, uint32_t cycle)
{
	if (!sound_log) return;
	if (!part && reg == 0x2A) { dac_writes++; return; } /* DAC samples: counted per frame */
	if (fprintf(sound_log, "{\"frame\":%u,\"cycle\":%u,\"chip\":\"ym2612\",\"port\":%u,\"reg\":%u,\"value\":%u}\n", log_frame, cycle, part, reg, value) < 0) log_failed = 1;
}

void sound_trace_psg(uint8_t value, uint32_t cycle)
{
	if (!sound_log) return;
	if (fprintf(sound_log, "{\"frame\":%u,\"cycle\":%u,\"chip\":\"psg\",\"value\":%u}\n", log_frame, cycle, value) < 0) log_failed = 1;
}

void sound_trace_frame_done(uint32_t frame)
{
	if (!sound_log) return;
	if (dac_writes && fprintf(sound_log, "{\"frame\":%u,\"dac_writes\":%u}\n", log_frame, dac_writes) < 0) log_failed = 1;
	dac_writes = 0;
	log_frame = frame + 1;
}

int sound_trace_close(void)
{
	if (!sound_log) return 0;
	int failed = log_failed || fflush(sound_log) || ferror(sound_log);
	failed |= fclose(sound_log) != 0;
	sound_log = NULL;
	return failed;
}
