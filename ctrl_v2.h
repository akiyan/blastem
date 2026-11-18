#ifndef CTRL_V2_H_
#define CTRL_V2_H_
#include <stddef.h>
void ctrl_v2_reset(void);
void ctrl_v2_frame_boundary(void);
void ctrl_v2_safe_stop(int stopped);
char *ctrl_v2_take_response(void);
char *ctrl_v2_command(const char *line, size_t length);
extern int ctrl_v2_debug_stopped;
extern int ctrl_v2_disconnect;
#endif
