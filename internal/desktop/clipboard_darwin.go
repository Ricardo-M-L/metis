//go:build darwin && cgo

package desktop

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework AppKit -framework Foundation
#import <AppKit/AppKit.h>
#import <Foundation/Foundation.h>
#include <stdlib.h>
#include <string.h>

static char *metis_clipboard_file_paths_json(void) {
	@autoreleasepool {
		NSPasteboard *pasteboard = [NSPasteboard generalPasteboard];
		NSDictionary *options = @{ NSPasteboardURLReadingFileURLsOnlyKey: @YES };
		NSArray *urls = [pasteboard readObjectsForClasses:@[[NSURL class]] options:options];
		NSMutableArray *paths = [NSMutableArray arrayWithCapacity:[urls count]];
		for (NSURL *url in urls) {
			if (![url isFileURL]) {
				continue;
			}
			NSString *path = [url path];
			if (path != nil) {
				[paths addObject:path];
			}
		}
		NSError *error = nil;
		NSData *data = [NSJSONSerialization dataWithJSONObject:paths options:0 error:&error];
		if (data == nil || error != nil) {
			return NULL;
		}
		char *result = malloc([data length] + 1);
		if (result == NULL) {
			return NULL;
		}
		memcpy(result, [data bytes], [data length]);
		result[[data length]] = '\0';
		return result;
	}
}

static void metis_clipboard_free(char *value) {
	free(value);
}
*/
import "C"

import (
	"encoding/json"
	"errors"
)

func platformClipboardFilePaths() ([]string, error) {
	raw := C.metis_clipboard_file_paths_json()
	if raw == nil {
		return nil, errors.New("read macOS clipboard file URLs")
	}
	defer C.metis_clipboard_free(raw)
	var paths []string
	if err := json.Unmarshal([]byte(C.GoString(raw)), &paths); err != nil {
		return nil, err
	}
	return paths, nil
}
