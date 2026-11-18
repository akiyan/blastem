"""Exercise the real Linux path functions without starting the emulator."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
CORE = ROOT


class StoragePaths(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory()
        cls.addClassCleanup(cls.temp.cleanup)
        directory = Path(cls.temp.name)
        harness = directory / "paths_probe.c"
        harness.write_text('''
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "paths.h"
char *alloc_concat(const char *a, const char *b) {
    char *p = malloc(strlen(a) + strlen(b) + 1);
    if (!p) abort();
    strcpy(p, a); strcat(p, b); return p;
}
int main(void) {
    puts(get_config_dir());
    puts(get_userdata_dir());
    return 0;
}
''')
        cls.binary = directory / "paths_probe"
        subprocess.run(["cc", "-std=gnu99", "-ffunction-sections", "-fdata-sections",
                        "-Wl,--gc-sections", "-I", str(CORE), str(CORE / "paths.c"),
                        str(harness), "-o", str(cls.binary)], check=True)

    def check_paths(self, config, data, expected_config, expected_data):
        env = dict(os.environ)
        for key, value in (("XDG_CONFIG_HOME", config), ("XDG_DATA_HOME", data)):
            env.pop(key, None)
            if value is not None:
                env[key] = value
        result = subprocess.check_output([str(self.binary)], env=env, text=True)
        self.assertEqual(result.splitlines(), [expected_config, expected_data])

    def test_absolute_overrides(self):
        self.check_paths("/tmp/custom config", "/tmp/custom data",
                         "/tmp/custom config/blastem", "/tmp/custom data")

    def test_fallbacks(self):
        for value in (None, "", "relative/path"):
            with self.subTest(value=value):
                self.check_paths(value, value, str(Path.home() / ".config/blastem"),
                                 str(Path.home() / ".local/share"))

    def test_independent_overrides(self):
        self.check_paths("/tmp/custom", None, "/tmp/custom/blastem",
                         str(Path.home() / ".local/share"))
        self.check_paths(None, "/tmp/custom", str(Path.home() / ".config/blastem"), "/tmp/custom")


if __name__ == "__main__":
    unittest.main()
