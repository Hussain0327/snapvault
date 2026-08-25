# Wi-Fi Router Manual - Model WR-900

This manual covers setup and configuration of the WR-900 dual-band router, part number WR900-US.

## Unboxing

The box includes the router, a power adapter rated at 12V 2A, one Ethernet cable, and a quick-start card.
The default IP address for the admin console is 192.168.1.1, printed on a sticker on the underside of the unit.

## Initial setup

Connect the WAN port to your modem using the included Ethernet cable, then power on the router.
Initial boot takes about 90 seconds, indicated by the status light changing from red to solid blue.

## Wi-Fi bands

The WR-900 broadcasts two networks by default: a 2.4 GHz band with a theoretical maximum of 300 Mbps, better for range, and a 5 GHz band rated up to 900 Mbps, better for speed at shorter distances.
Both networks share the same password by default but can be split into separate SSIDs in the admin console.

## Admin console

Log into 192.168.1.1 with the default username "admin" and the password printed on the router's label; change this password immediately, since routers left on default credentials account for a large share of home network intrusions according to the included security notice.

## Port forwarding

Up to 32 port forwarding rules can be configured under the Advanced tab.
Each rule requires an internal IP address, an internal port, and an external port; the router does not validate that the internal IP is actually reachable, so a typo here is a common cause of forwarding rules that silently fail.

## Firmware updates

The WR-900 checks for firmware updates automatically every 7 days when connected to the internet, or updates can be triggered manually from the System tab.
Do not power off the router during an update, which typically takes 3 to 4 minutes and will corrupt the firmware if interrupted.

## Warranty

The WR-900 is covered by a 2-year limited warranty from the date of purchase, valid only when registered within 90 days.
