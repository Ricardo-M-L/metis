#ifndef METIS_NATIVE_DOCK_ICON_H
#define METIS_NATIVE_DOCK_ICON_H

#include <stddef.h>

// The caller may release the PNG bytes as soon as this function returns.
void metis_install_dock_icon(const void *bytes, size_t length);
void metis_refresh_dock_icon(void);

#endif
