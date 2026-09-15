# Startup image

A deployment can show a centered image while a quiet interactive `wippy run`
loads. Configure a local file in the merged runtime configuration:

```yaml
terminal:
  splash: assets/startup.png
```

The path is relative to the working directory. `--set terminal.splash=` disables
it for one launch. The file belongs to the deployment: module filesystems are
not available yet. PNG, JPEG and the first frame of GIF are supported. Images
are limited to 16 MiB / 16 megapixels. Missing or invalid files report an error
in interactive use.

The image keeps its proportions inside 640×480 pixels, shrinking when needed
to leave a one-cell margin around the terminal; small images are not enlarged. The rest of the screen is
black. Size is determined at startup. No animation or artificial delay is added.

The CLI uses the terminal service's existing capability probe and graphics
renderer. It skips the splash for redirected input/output, verbose logging,
`wippy test`, or terminals lacking graphics or known cell geometry. Local,
locked Hub and pack startup paths share the same setting. For explicit pack
launches, pass `--silent` if the invocation does not already suppress logs.

A fullscreen application takes ownership of the existing physical surface. Its
first frame removes the startup placement in the same transaction; the alternate
screen is not re-entered. Inline surfaces dismiss the splash when they open.
Without a terminal command, it is dismissed when registry loading is complete.
Errors and interrupts during component loading restore the original console.
The normal surface owner performs cleanup after handoff.
