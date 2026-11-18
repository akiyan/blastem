"""Exercise the actual core WAV writer, including failed output finalization."""
from pathlib import Path
import subprocess
import tempfile
import unittest
import wave

ROOT=Path(__file__).resolve().parents[1]

class WaveWriter(unittest.TestCase):
    def test_header_samples_and_failure(self):
        core=ROOT
        with tempfile.TemporaryDirectory() as directory:
            work=Path(directory)
            source=work/'test.c'
            source.write_text('''#include "wave.h"
#include <assert.h>
int main(int argc, char **argv) {
 FILE *f=fopen(argv[1],"wb"); assert(f);
 assert(wave_init(f,48000,16,2));
 int16_t samples[4]={1,-1,32767,-32768};
 assert(fwrite(samples,sizeof(samples),1,f)==1);
 assert(wave_finalize(f));
 f=fopen("/dev/full","wb"); assert(f);
 wave_init(f,48000,16,2);
 assert(!wave_finalize(f));
 return 0;
}
''')
            subprocess.run(['cc','-I',str(core),str(source),str(core/'wave.c'),'-o',str(work/'test')],check=True)
            subprocess.run([str(work/'test'),str(work/'test.wav')],check=True)
            with wave.open(str(work/'test.wav')) as wav:
                self.assertEqual((wav.getnchannels(),wav.getsampwidth(),wav.getframerate(),wav.getnframes()),(2,2,48000,2))
                self.assertEqual(len(wav.readframes(2)),8)
