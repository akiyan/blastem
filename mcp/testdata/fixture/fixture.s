/*
 * BlastEM MCP deterministic integration fixture.
 * SPDX-License-Identifier: MIT
 *
 * The ROM initializes a Mega Drive VDP with known tiles and four deliberately
 * distinct CRAM palettes. It polls controller port 1 once per frame and stores
 * stable test markers in work RAM for MCP memory/debug assertions.
 */

	.equ VDP_DATA,       0x00C00000
	.equ VDP_CTRL,       0x00C00004
	.equ IO_VERSION,     0x00A10001
	.equ PAD1_DATA,      0x00A10003
	.equ PAD1_CTRL,      0x00A10009
	.equ TMSS,           0x00A14000
	.equ Z80_RAM,        0x00A00000
	.equ Z80_BUSREQ,     0x00A11100
	.equ Z80_RESET,      0x00A11200

	.equ fixture_magic,         0x00FF0000
	.equ fixture_frame_counter, 0x00FF0004
	.equ fixture_input_state,   0x00FF0008
	.equ fixture_raw_high,      0x00FF000A
	.equ fixture_raw_low,       0x00FF000B
	.equ fixture_scratch,       0x00FF0010
	.equ fixture_hint_count,    0x00FF0024

	.section .vectors,"a"
	.long 0x00FFFFFC
	.long reset
	.rept 26
	.long exception_handler
	.endr
	.long hint_handler                       /* level 4: horizontal interrupt */
	.long exception_handler
	.long vint_handler                       /* level 6: vertical interrupt */
	.rept 33
	.long exception_handler
	.endr

	.section .header,"a"
	.ascii "SEGA MEGA DRIVE "
	.ascii "(C)OPENAI 2026  "
	.ascii "BLASTEM MCP INTEGRATION FIXTURE                 "
	.ascii "BLASTEM MCP INTEGRATION FIXTURE                 "
	.ascii "GM 00000000-00"
	.word 0x0000
	.ascii "J               "
	.long 0x00000000
	.long 0x0000FFFF
	.long 0x00FF0000
	.long 0x00FFFFFF
	.ascii "RA"
	.byte 0xF8, 0x20
	.long 0x00200001
	.long 0x0020FFFF
	.space 12, 0x20
	.ascii "OPEN SOURCE DETERMINISTIC MCP TEST ROM  "
	.ascii "JUE             "

	.section .text,"ax"
	.global reset
	.global fixture_breakpoint
	.global fixture_frame_counter
	.global fixture_input_state
	.global fixture_scratch
	.global fixture_hint_count

reset:
	move.w #0x2700, %sr
	lea 0x00FFFFFC, %sp

	/* Unlock VDP access on systems with TMSS. */
	move.b IO_VERSION, %d0
	andi.b #0x0F, %d0
	beq.s 1f
	move.l #0x53454741, TMSS
1:
	move.l #0x424D4350, fixture_magic       /* "BMCP" */
	clr.l fixture_frame_counter
	clr.l fixture_input_state
	move.l #0x11223344, fixture_scratch
	move.b #1, 0x00A130F1                 /* enable odd-byte SRAM */
	move.b 0x00200001, 0x00FF0020         /* saved marker at boot */

	/* Configure pad 1 TH as output and leave it high. */
	move.b #0x40, PAD1_CTRL
	move.b #0x40, PAD1_DATA

	move.b #0x80, 0x00C00011 /* PSG tone 0 period 256: about 437 Hz */
	move.b #0x10, 0x00C00011
	move.b #0x9F, 0x00C00011 /* initially silent */
	bsr.w load_z80
	bsr.w init_vdp
	bsr.w load_cram
	bsr.w load_tiles
	bsr.w load_plane
	move.w #0x8174, VDP_CTRL               /* display on */

main_loop:
	/* Wait for a complete VBlank edge. */
wait_active:
	move.w VDP_CTRL, %d0
	btst #3, %d0
	bne.s wait_active
wait_vblank:
	move.w VDP_CTRL, %d0
	btst #3, %d0
	beq.s wait_vblank

fixture_breakpoint:
	nop
	addq.l #1, fixture_frame_counter
	bsr.w read_pad1
	bra.s main_loop

init_vdp:
	lea vdp_registers, %a0
	moveq #18, %d0
1:
	move.w (%a0)+, VDP_CTRL
	dbra %d0, 1b

	/* Clear all 64 KiB of VRAM. */
	move.l #0x40000000, VDP_CTRL
	moveq #0, %d0
	move.w #0x7FFF, %d1
2:
	move.w %d0, VDP_DATA
	dbra %d1, 2b
	rts

load_cram:
	move.l #0xC0000000, VDP_CTRL
	lea cram_data, %a0
	moveq #63, %d0
1:
	move.w (%a0)+, VDP_DATA
	dbra %d0, 1b
	rts

load_tiles:
	move.l #0x40000000, VDP_CTRL
	lea tile_data, %a0
	move.w #255, %d0                         /* 16 tiles * 16 words */
1:
	move.w (%a0)+, VDP_DATA
	dbra %d0, 1b
	rts

load_plane:
	move.l #0x60000003, VDP_CTRL             /* VRAM 0xE000 */
	lea plane_data, %a0
	move.w #1119, %d0                        /* 40 x 28 entries */
1:
	move.w (%a0)+, VDP_DATA
	dbra %d0, 1b
	rts

read_pad1:
	move.b #0x40, PAD1_DATA
	nop
	nop
	move.b PAD1_DATA, %d0                    /* UDLRBC, active low */
	move.b %d0, fixture_raw_high
	move.b #0x00, PAD1_DATA
	nop
	nop
	move.b PAD1_DATA, %d1                    /* __SAUD, active low */
	move.b %d1, fixture_raw_low
	move.b #0x40, PAD1_DATA

	not.b %d0
	andi.w #0x003F, %d0
	not.b %d1
	move.w %d0, %d2
	btst #4, %d1
	beq.s 1f
	bset #6, %d2                             /* A */
1:
	btst #5, %d1
	beq.s 2f
	bset #7, %d2                             /* Start */
2:
	move.w %d2, fixture_input_state

	/* Holding B requests the Z80 bus, stopping the Z80 counter program. */
	btst #4, %d2
	beq.s 6f
	move.w #0x0100, Z80_BUSREQ
	bra.s 7f
6:
	move.w #0x0000, Z80_BUSREQ
7:

	/* Holding Start switches the display to H32 (256 pixels wide). */
	btst #7, %d2
	beq.s 8f
	move.w #0x8C00, VDP_CTRL
	bra.s 9f
8:
	move.w #0x8C81, VDP_CTRL
9:

	/* Holding C enables a raster test: an interrupt on every line writes CRAM entry 0. */
	btst #5, %d2
	beq.s 4f
	move.w #0x8A00, VDP_CTRL                 /* interrupt every line */
	move.w #0x8014, VDP_CTRL                 /* horizontal interrupt on */
	move.w #0x2300, %sr
	bra.s 5f
4:
	move.w #0x2700, %sr
	move.w #0x8004, VDP_CTRL
5:

	/* Make A input visible in the border/backdrop as well as in RAM. */
	btst #6, %d2
	beq.s 3f
	move.b #0x94, 0x00C00011 /* A enables the fixture tone */
	move.w #0x8702, VDP_CTRL                 /* palette 0, red */
	move.b #0xA5, 0x00200001                /* observable battery save */
	rts
3:
	move.b #0x9F, 0x00C00011 /* releasing A silences tone */
	move.w #0x8701, VDP_CTRL                 /* palette 0, white */
	rts

exception_handler:
	bra.s exception_handler

/* Copy a Z80 loop that increments Z80 RAM 0x1000 forever, then start the Z80. */
load_z80:
	move.w #0x0100, Z80_BUSREQ
	move.w #0x0100, Z80_RESET
1:
	btst #0, Z80_BUSREQ
	bne.s 1b
	lea z80_program, %a0
	lea Z80_RAM, %a1
	moveq #z80_program_end - z80_program - 1, %d0
2:
	move.b (%a0)+, (%a1)+
	dbra %d0, 2b
	move.w #0x0000, Z80_RESET
	move.w #0x0000, Z80_BUSREQ
	move.w #0x0100, Z80_RESET
	rts

z80_program:
	.byte 0x21, 0x00, 0x10                   /* ld hl,0x1000 */
	.byte 0x34                               /* loop: inc (hl) */
	.byte 0x18, 0xFD                         /* jr loop */
z80_program_end:
	.align 2

hint_handler:
	addq.w #1, fixture_hint_count
	move.l #0xC0000000, VDP_CTRL             /* CRAM entry 0: never displayed */
	move.w fixture_hint_count, VDP_DATA
	rte

vint_handler:
	rte

	.align 2
vdp_registers:
	.word 0x8004, 0x8134, 0x8238, 0x8330, 0x8406
	.word 0x856C, 0x8600, 0x8701, 0x8800, 0x8900
	.word 0x8A01, 0x8B00, 0x8C81, 0x8D3B, 0x8E00
	.word 0x8F02, 0x9001, 0x9100, 0x9200

	/* Four unmistakable 16-colour palettes: RGB, CMY, grayscale, mixed. */
cram_data:
	.word 0x0000,0x0EEE,0x000E,0x00E0,0x0E00,0x00EE,0x0E0E,0x0EE0
	.word 0x0222,0x0444,0x0666,0x0888,0x0AAA,0x0CCC,0x0E88,0x088E
	.word 0x0000,0x0002,0x0004,0x0006,0x0008,0x000A,0x000C,0x000E
	.word 0x0020,0x0040,0x0060,0x0080,0x00A0,0x00C0,0x00E0,0x0EEE
	.word 0x0000,0x0200,0x0400,0x0600,0x0800,0x0A00,0x0C00,0x0E00
	.word 0x0222,0x0444,0x0666,0x0888,0x0AAA,0x0CCC,0x0EEE,0x0ACE
	.word 0x0000,0x000E,0x00E0,0x0E00,0x00EE,0x0E0E,0x0EE0,0x0EEE
	.word 0x0246,0x0468,0x068A,0x08AC,0x0ACE,0x0C8A,0x0E64,0x084E

	/* Tile 0 is transparent; tiles 1-15 are solid colour indices. */
tile_data:
	.rept 8
	.long 0x00000000
	.endr
	.set colour, 1
	.rept 15
	.rept 8
	.long (colour * 0x11111111)
	.endr
	.set colour, colour + 1
	.endr

	/* Four seven-row bands select palettes 0, 1, 2, and 3. */
plane_data:
	.set row, 0
	.rept 28
	.set column, 0
	.rept 40
	.word (((row / 7) * 0x2000) + column - ((column / 15) * 15) + 1)
	.set column, column + 1
	.endr
	.set row, row + 1
	.endr
