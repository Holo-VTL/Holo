# Integration Test: Host Discovery Login Windows

## Steps
1. Open `iscsicpl` and add target portal (`<server-ip>:3260`).
2. Refresh discovered targets and find published IQN.
3. Connect target with Quick Connect or Connect.
4. Verify new device in Disk Management or Device Manager.

## Expected
- target IQN is discoverable in Windows initiator
- target can connect successfully
- host device list shows new iSCSI-attached device
