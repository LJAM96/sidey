Sidey for iOS: Native companion app proposal
This is a very feasible addition to LJAM96/sidey, and I would build it as a native SwiftUI application within the existing repository, rather than as a separate project.
Having inspected the repository, Sidey already has much of the server infrastructure needed for this. The iOS application can become a mobile management interface for your existing Sidey instance, while the server continues handling Apple authentication, signing, refresh scheduling and device communication.
There are two distinct capabilities worth pursuing.
Remote management: View installed applications, monitor signing expiry, initiate refreshes, browse available applications, manage updates and select releases for installation.
Local signing and installation: Explore whether Sidey can sign applications or manage installations directly from the iPhone without relying on the server. This is substantially more complicated because of iOS security restrictions, so I would treat it as a separate development phase.
Proposed application



A genuinely native iOS experience
Use SwiftUI navigation, lists, forms, sheets, toolbars, search and system controls. Allow iOS to provide its own Liquid Glass appearance rather than manually reproducing it.



I would organise the application around four main destinations, with Settings available from the main interface.
Home
Server health, device status, upcoming expirations, active signing jobs and applications needing attention.



My Apps
Applications installed on each managed device, version information, expiry dates, refresh controls and installation history.



Browse
Available apps from Sidey's sources, application details, version selection, release channels and import actions.



Updates
Detected newer versions, release comparisons, available signing actions and progress monitoring.



This structure gives Sidey a proper iPhone experience without attempting to squeeze the entire desktop dashboard onto a smaller screen.
What Sidey already supports
The good news is that this isn't starting from scratch. These are actual endpoints and capabilities I found in the code.
Capability	Current state
View devices and deployments	Implemented
View signing jobs and their status	Implemented
See profile expiry and refresh status	Implemented
Manually trigger a refresh	Implemented
Browse available applications	Implemented
Browse GitHub and AltStore sources	Implemented
Show stable, beta, alpha and nightly releases	Partially implemented
Import and deploy a selected IPA	Implemented
Detect application updates	Implemented, with limitations
Perform local signing on iPhone	Not implemented
The relevant existing files are api.go, store.go and device_refresh.go.
Release selection needs some improvements
I found that Sidey's StoreApp model already contains a versions array with channel labels for stable, beta, nightly and alpha.
That means the iOS application could use a native SwiftUI Picker to let you choose a release instead of automatically accepting the latest stable build.
There are, however, some limitations in the current implementation.
GitHub releases: The code retrieves release assets and classifies pre-releases, but currently limits the returned version list to six entries.
AltStore sources: The parser currently reads the application's top level version fields rather than fully handling the modern versions[] history.
Update detection: The current snapshot compares versions primarily using bundle identifiers. GitHub listings use generated identifiers, which may differ from the actual IPA bundle identifier. This needs correcting for reliable update tracking.
I would fix those issues in the server rather than introduce separate release handling inside SwiftUI.
Native SwiftUI implementation
Apple's standard SwiftUI components already provide nearly all of the interface behaviour required. Building with the current SDK lets the system supply its native appearance, including Liquid Glass where appropriate. 

Apple Developer Documentation



Requirement	Native implementation
Main navigation	TabView
Screen navigation	NavigationStack
App and device lists	List
Settings	Form
Search	.searchable()
Reloading	.refreshable()
Version selection	Picker
Import local IPA	fileImporter
Signing progress	ProgressView
Confirm installations	confirmationDialog
Icons	SF Symbols
Server communication	URLSession
Secure credential storage	Keychain Services
No third party UI framework is necessary.
I would create a new ios/ directory containing the Xcode project, SwiftUI views, API client and data models. The Go API remains the authoritative backend. Both interfaces would consume the same backend state and trigger the same jobs.
On device signing


Worth investigating, but separately
Feather demonstrates that signing IPAs directly on an iPhone is possible using signing certificates and provisioning profiles. Its source is available for technical reference, although its GPLv3 licensing needs consideration before reusing code. 

GitHub





The important distinction is that signing an IPA and installing it are different operations.
An iOS app can potentially sign an IPA locally using appropriate cryptographic code. However, iOS doesn't provide an unrestricted public API for an ordinary application to install arbitrary other applications. Solutions such as Feather use specialised installation methods with their own compatibility requirements.
I would therefore support two execution modes.
Server signing, primary mode
Select an app and release on the iPhone, send an instruction to Sidey, sign on the existing worker and install using the device service. This reuses the current infrastructure.



Local signing, experimental mode
Import an IPA, certificate and provisioning profile, sign on the device, and export the resulting IPA or use a separately validated installation mechanism. The signing engine could be shared native code, but would require significant additional engineering.



Important security issue in the current API
While inspecting the repository, I also noticed something that should be fixed before adding a mobile client.
In internal/api/api.go, the /api/v1/config handler returns the configured admin API key without requiring authentication. The web route also sets the admin key as a session cookie.
Additionally, the admin middleware accepts requests based on the presence of certain client supplied headers rather than validating credentials.
These are serious authentication vulnerabilities if the control plane is reachable by an untrusted client. The supplied Caddy configuration does not appear to enforce additional authentication.
I would address this before further internet facing deployment, restrict network access in the meantime, and rotate the admin credential after fixing the exposure.
For the iOS client, introduce independently revocable credentials with appropriate permissions, store tokens in the iOS Keychain, and use HTTPS. 

Apple Developer Documentation



Recommended development sequence
Foundation
Fix API authentication, define stable mobile API responses, establish server pairing and build a minimal native SwiftUI application.



Remote management
Add device information, managed applications, signing history, expiry monitoring, manual refresh and job progress.



Application management
Add repository browsing, release channels, release selection, version comparisons, import approval and installation actions.



Advanced capabilities
Investigate local IPA signing, installation mechanisms, optional notifications, offline viewing and handling the companion app's own provisioning expiry.



My preferred scope for the first release is a full native remote control for Sidey, including browsing, version selection, signing and refreshing through the server. That would deliver most of the functionality you described while preserving the existing signing architecture.
I would then investigate local signing as an optional capability rather than making it a dependency of the core application. The existing sidey repository is the right place for both the client and the supporting API changes.
No repository changes have been made.
