// Public-release shim.
//
// The module-side call runtime is intentionally not part of this repository
// or its release packages.  The Swift app retains the MIT-licensed host-side
// MaVo implementation for review, but the legacy Go audio bridge must remain
// inert in public builds.  These symbols keep the macOS control plane
// buildable without implying an included voice route.

#include <stdint.h>
#include <stddef.h>
#include <stdio.h>

void *dj_mavo_uac_bridge_start(void *context, uint16_t vendor, uint16_t product,
                               uint32_t location, char *error, size_t error_capacity) {
    (void)context; (void)vendor; (void)product; (void)location;
    if (error && error_capacity > 0) {
        snprintf(error, error_capacity, "%s", "公开源码版未包含模块侧语音运行时");
    }
    return NULL;
}

void dj_mavo_uac_bridge_stop(void *bridge) { (void)bridge; }
void dj_mavo_uac_bridge_set_muted(void *bridge, int muted) { (void)bridge; (void)muted; }
int dj_mavo_uac_bridge_running(void *bridge) { (void)bridge; return 0; }
void dj_mavo_uac_bridge_stats(void *bridge, uint64_t *input_callbacks,
                              uint64_t *output_callbacks, uint64_t *input_frames,
                              uint64_t *output_frames) {
    (void)bridge;
    if (input_callbacks) *input_callbacks = 0;
    if (output_callbacks) *output_callbacks = 0;
    if (input_frames) *input_frames = 0;
    if (output_frames) *output_frames = 0;
}
const char *dj_mavo_uac_bridge_name(void *bridge) { (void)bridge; return ""; }
