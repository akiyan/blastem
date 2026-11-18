/* Per-channel muting and register-write logging for sound analysis. Same GPL-3.0-or-later license as BlastEM. */
#ifndef SOUND_TRACE_H_
#define SOUND_TRACE_H_
#include <stdint.h>
#include <stdio.h>

/* Bits: 0-5 FM channels 1-6, 6 DAC (replaces FM6 while enabled), 7-10 PSG tone 1-3 and noise. */
enum {
	SOUND_FM1, SOUND_FM2, SOUND_FM3, SOUND_FM4, SOUND_FM5, SOUND_FM6, SOUND_DAC,
	SOUND_PSG1, SOUND_PSG2, SOUND_PSG3, SOUND_NOISE, SOUND_CHANNELS
};
extern uint16_t sound_mute_mask;
extern FILE *sound_log;

/* Parse "fm1,dac,psg,..." (groups fm, psg) into a mask; returns 0 on error. */
int sound_parse_channels(const char *list, uint16_t *mask);
void sound_mute_names(char *out, size_t size);
void sound_trace_ym(uint8_t part, uint8_t reg, uint8_t value, uint32_t cycle);
void sound_trace_psg(uint8_t value, uint32_t cycle);
/* Called after each completed video frame; frame is the completed-frame count.
 * Log records carry the 1-based number of the frame whose emulation produced them. */
void sound_trace_frame_done(uint32_t frame);
int sound_trace_close(void);
#endif
