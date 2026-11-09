# Windows console provider

On Windows, DCP uses the operating system's ConPTY implementation by default. An AppHost or another launcher can opt into a standalone implementation by setting `DCP_CONPTY_PATH` in DCP's environment before starting DCP. The value is the directory containing `conpty.dll`, not the path to a DLL or executable. DCP does not download, pin, or redistribute this payload; its provider owns distribution, updates, and licensing.

An unset or empty variable selects inbox ConPTY. A nonempty value selects standalone ConPTY; missing binaries, invalid DLLs, and missing exports produce errors rather than silently falling back to inbox ConPTY. Selection and DLL loading happen once per process, on the first terminal launch, and the result is retained for that process's lifetime. Each PTY uses the same provider for creation, resizing, and closure. Non-Windows platforms ignore this variable.

The standalone DLL must export `ConptyCreatePseudoConsole`, `ConptyResizePseudoConsole`, and `ConptyClosePseudoConsole`. The DLL must match DCP's process architecture; `OpenConsole.exe` must match the native Windows architecture. Keep hosts in the following subdirectories of `DCP_CONPTY_PATH` to support emulation:

| DCP `GOARCH` | DLL architecture | Host paths for supported native Windows architectures |
| --- | --- | --- |
| `amd64` | x64 | `x64/OpenConsole.exe`, `arm64/OpenConsole.exe` |
| `arm64` | arm64 | `arm64/OpenConsole.exe` |
| `386` | x86 | `x86/OpenConsole.exe`, `x64/OpenConsole.exe`, `arm64/OpenConsole.exe` |

Only the host for the current native Windows architecture is required at runtime. Do not place `OpenConsole.exe` directly beside `conpty.dll`: that overrides native-host selection and is rejected. DLL dependencies are resolved from the configured directory and System32, not arbitrary search-path directories. Use a trusted payload directory and keep its files available for the lifetime of DCP.

For example, with an already restored Hex1b Windows x64 payload:

```powershell
$env:DCP_CONPTY_PATH = 'C:\Nuget\hex1b\<version>\runtimes\win-x64\native'
```

Set this on the DCP process, not just the terminalized workload. Use an absolute path so child DCP processes resolve the same directory. After `make test-prereqs`, run `go test -count 1 -parallel 32 -timeout 180s ./internal/termpty` with the variable unset to exercise inbox ConPTY, and with it set to exercise the external provider, including KGP and Sixel passthrough. Graphics support depends on the supplied implementation; standalone-only tests are skipped when no payload is configured.