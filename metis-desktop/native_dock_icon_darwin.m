#import <Cocoa/Cocoa.h>
#import <dispatch/dispatch.h>
#import "native_dock_icon_darwin.h"

static void *MetisDockAppearanceContext = &MetisDockAppearanceContext;

@interface MetisDockIconController : NSObject {
	NSImage *_lightIcon;
	NSImage *_darkIcon;
	BOOL _observing;
	BOOL _hasAppearance;
	BOOL _lastDark;
}
- (instancetype)initWithPNGData:(NSData *)data;
- (void)refresh;
@end

// Render the familiar rounded-square macOS app tile ourselves, so the running
// Dock does not add a second compatibility rim. Keep the logo as a separate
// transparent layer: dark-mode contrast must not brighten the tile background.
static NSImage *MetisRenderDockTile(NSImage *mark, BOOL dark) {
 const NSInteger side = 1024;
 NSBitmapImageRep *(^bitmap)(void) = ^{
  return [[[NSBitmapImageRep alloc] initWithBitmapDataPlanes:NULL
   pixelsWide:side pixelsHigh:side bitsPerSample:8 samplesPerPixel:4
   hasAlpha:YES isPlanar:NO colorSpaceName:NSDeviceRGBColorSpace
   bytesPerRow:0 bitsPerPixel:0] autorelease];
 };
 NSBitmapImageRep *inkBitmap = bitmap();
 NSBitmapImageRep *tileBitmap = bitmap();
 if (inkBitmap == nil || tileBitmap == nil) return nil;
 NSRect canvas = NSMakeRect(0, 0, side, side);
 [NSGraphicsContext saveGraphicsState];
 [NSGraphicsContext setCurrentContext:[NSGraphicsContext graphicsContextWithBitmapImageRep:inkBitmap]];
 [mark drawInRect:NSMakeRect(80, 80, 864, 864) fromRect:NSZeroRect
  operation:NSCompositingOperationCopy fraction:1.0];
 if (dark) {
  [[NSColor colorWithDeviceWhite:1.0 alpha:0.18] setFill];
  NSRectFillUsingOperation(canvas, NSCompositingOperationSourceAtop);
 }
 [NSGraphicsContext restoreGraphicsState];
 NSImage *ink = [[NSImage alloc] initWithSize:NSMakeSize(side,side)];
 [ink addRepresentation:inkBitmap];

 [NSGraphicsContext saveGraphicsState];
 [NSGraphicsContext setCurrentContext:[NSGraphicsContext graphicsContextWithBitmapImageRep:tileBitmap]];
 NSBezierPath *tile = [NSBezierPath bezierPathWithRoundedRect:NSMakeRect(96,96,832,832)
  xRadius:180 yRadius:180];
 NSColor *background = dark
  ? [NSColor colorWithDeviceRed:37.0/255 green:39.0/255 blue:45.0/255 alpha:1.0]
  : [NSColor colorWithDeviceRed:243.0/255 green:244.0/255 blue:247.0/255 alpha:1.0];
 [background setFill];
 [tile fill];
 [tile addClip];
 [ink drawInRect:canvas fromRect:NSZeroRect operation:NSCompositingOperationSourceOver fraction:1.0];
 [NSGraphicsContext restoreGraphicsState];
 [ink release];
 NSImage *result = [[NSImage alloc] initWithSize:NSMakeSize(side,side)];
 [result addRepresentation:tileBitmap];
 return result;
}

@implementation MetisDockIconController

- (instancetype)initWithPNGData:(NSData *)data {
 self = [super init];
 if (self == nil) return nil;
 NSImage *mark = [[NSImage alloc] initWithData:data];
 if (mark != nil) {
  _lightIcon = MetisRenderDockTile(mark, NO);
  _darkIcon = MetisRenderDockTile(mark, YES);
 }
 [mark release];
 if (_lightIcon == nil || _darkIcon == nil) {
  [self release];
  return nil;
 }

	if (@available(macOS 10.14, *)) {
		[NSApp addObserver:self forKeyPath:@"effectiveAppearance"
			options:NSKeyValueObservingOptionNew context:MetisDockAppearanceContext];
		_observing = YES;
	}
	return self;
}

- (void)refresh {
	NSAssert([NSThread isMainThread], @"Dock icon updates must run on the main thread");
	BOOL dark = NO;
	if (@available(macOS 10.14, *)) {
		NSString *match = [[NSApp effectiveAppearance]
			bestMatchFromAppearancesWithNames:@[NSAppearanceNameAqua, NSAppearanceNameDarkAqua]];
		dark = [match isEqualToString:NSAppearanceNameDarkAqua];
	}
	NSImage *icon = dark ? _darkIcon : _lightIcon;
	if (_hasAppearance && _lastDark == dark) return;
	_hasAppearance = YES;
	_lastDark = dark;
	// applicationIconImage controls the running Dock tile directly. Do not use
	// NSRunningApplication.icon: that property returns the static bundle icon.
	[NSApp setApplicationIconImage:icon];
	[[NSApp dockTile] display];
}

- (void)observeValueForKeyPath:(NSString *)keyPath ofObject:(id)object
	change:(NSDictionary *)change context:(void *)context {
	if (context != MetisDockAppearanceContext) {
		[super observeValueForKeyPath:keyPath ofObject:object change:change context:context];
		return;
	}
	if ([NSThread isMainThread]) {
		[self refresh];
	} else {
		dispatch_async(dispatch_get_main_queue(), ^{ [self refresh]; });
	}
}

- (void)dealloc {
	if (_observing) [NSApp removeObserver:self forKeyPath:@"effectiveAppearance" context:MetisDockAppearanceContext];
	[_lightIcon release];
	[_darkIcon release];
	[super dealloc];
}
@end

// Process lifetime, deliberately initialized only once on the main queue.
static MetisDockIconController *MetisDockIcon;

void metis_install_dock_icon(const void *bytes, size_t length) {
	if (bytes == NULL || length == 0) return;
	NSData *data = [[NSData alloc] initWithBytes:bytes length:length];
	dispatch_async(dispatch_get_main_queue(), ^{
		if (MetisDockIcon == nil && NSApp != nil) {
			MetisDockIcon = [[MetisDockIconController alloc] initWithPNGData:data];
		}
		[MetisDockIcon refresh];
		[data release];
	});
}

void metis_refresh_dock_icon(void) {
	if ([NSThread isMainThread]) {
		[MetisDockIcon refresh];
	} else {
		dispatch_async(dispatch_get_main_queue(), ^{ [MetisDockIcon refresh]; });
	}
}
