use data_plane::{
    iscsi::cdb_server::{
        dispatch_raw_cdb, CdbResponse, SCSI_STATUS_CHECK_CONDITION, SCSI_STATUS_GOOD,
    },
    media::mount_bridge::{attach_cartridge, detach_cartridge},
    scsi_tape::state::TapeState,
    storage::storage_root_dir_for_cartridge,
};
use std::{env, fs, path::PathBuf, time::SystemTime};

struct Scratch(PathBuf);

impl Drop for Scratch {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}

struct Drive(TapeState);

impl Drop for Drive {
    fn drop(&mut self) {
        detach_cartridge(&mut self.0).expect("detach diagnostic cartridge");
    }
}

fn good(state: &mut TapeState, cdb: &[u8], data: &[u8]) -> CdbResponse {
    let response = dispatch_raw_cdb(state, cdb, data);
    assert_eq!(
        response.status, SCSI_STATUS_GOOD,
        "cdb={cdb:?}, {response:?}"
    );
    response
}

fn mode(state: &mut TapeState, size: u32) {
    let mut parameters = [0; 12];
    parameters[3] = 8;
    parameters[9..12].copy_from_slice(&size.to_be_bytes()[1..]);
    good(state, &[0x15, 0x10, 0, 0, 12, 0], &parameters);
    while state.take_unit_attention().is_some() {}
}

fn read_cdb(opcode: u8) -> Vec<u8> {
    let mut cdb = vec![0; if opcode == 0x08 { 6 } else { 16 }];
    cdb[0] = opcode;
    if opcode == 0x08 {
        cdb[4] = 4;
    } else {
        cdb[13] = 4;
    }
    cdb
}

fn filemark(state: &mut TapeState, cdb: &[u8]) {
    let response = dispatch_raw_cdb(state, cdb, &[]);
    assert_eq!(response.status, SCSI_STATUS_CHECK_CONDITION);
    assert_eq!(response.sense[2], 0x80);
    assert_eq!(&response.sense[12..14], &[0, 1]);
    assert!(response.reply.is_empty());
}

#[test]
fn forward_space_uses_stored_filemark_boundaries() {
    let nonce = SystemTime::now()
        .duration_since(SystemTime::UNIX_EPOCH)
        .expect("clock")
        .as_nanos();
    let scratch = Scratch(env::temp_dir().join(format!("holo-space52-{nonce}")));
    let media = scratch.0.join("state");
    fs::create_dir_all(&media).expect("isolated media state");
    // This integration-test process has one test and no shared environment fixtures.
    env::set_var("HOLO_STORAGE_ROOT", scratch.0.join("storage"));
    env::set_var("HOLO_MEDIA_STATE_DIR", &media);
    env::set_var("HOLO_MEDIA_STATE_KEY", "space52");
    env::set_var("HOLO_LAYOUT_LIBRARY_ID", "space52");
    env::set_var("HOLO_RUN_DIR", scratch.0.join("run"));
    env::set_var("HOLO_TAPE_DRIVE_PROFILE", "ibm-ult3580-td6");

    for written_size in [4u32, 0] {
        let span = u64::from(written_size.max(1));
        for selected_size in [0u32, 2, 4, 8] {
            for opcode in [0x08, 0x88] {
                for count in [1u8, 2] {
                    let case = format!("{written_size}-{selected_size}-{opcode}-{count}");
                    let cartridge = format!("cart-{case}");
                    fs::create_dir_all(storage_root_dir_for_cartridge(&cartridge))
                        .expect("isolated storage root");
                    fs::write(
                        media.join("space52.state"),
                        format!("cartridge={cartridge}\n"),
                    )
                    .expect("loaded diagnostic cartridge");
                    let mut drive = Drive(TapeState::new("space52-drive"));
                    let state = &mut drive.0;
                    attach_cartridge(state, &cartridge).expect("attach diagnostic cartridge");
                    while state.take_unit_attention().is_some() {}
                    mode(state, written_size);
                    let write = [
                        0x0a,
                        u8::from(written_size != 0),
                        0,
                        0,
                        if written_size == 0 { 4 } else { 1 },
                        0,
                    ];
                    for payload in [b"AAAA", b"BBBB"] {
                        good(state, &write, payload);
                    }
                    good(state, &[0x10, 0, 0, 0, 2, 0], &[]);
                    good(state, &write, b"CCCC");
                    good(state, &[0x10, 0, 0, 0, 2, 0], &[]);
                    good(state, &[1, 0, 0, 0, 0, 0], &[]);
                    mode(state, selected_size);

                    good(state, &[0x11, 1, 0, 0, count, 0], &[]);
                    assert_eq!(
                        state.current_position,
                        8 + u64::from(count) * span,
                        "{case}"
                    );
                    // Read the stored four-byte record in variable mode, regardless
                    // of the transfer mode used to cross the filemarks.
                    mode(state, 0);
                    let read = read_cdb(opcode);
                    if count == 1 {
                        filemark(state, &read);
                    }
                    assert_eq!(good(state, &read, &[]).reply, b"CCCC", "{case}");

                    mode(state, selected_size);
                    good(state, &[0x11, 1, 0, 0, 0, 0], &[]);
                    assert_eq!(state.current_position, 12 + 2 * span, "{case}");
                    good(state, &[0x11, 1, 0, 0, 1, 0], &[]);
                    assert_eq!(state.current_position, 12 + 3 * span, "{case}");
                    mode(state, 0);
                    filemark(state, &read);
                    assert_eq!(state.current_position, state.eod_position, "{case}");
                    let eod = dispatch_raw_cdb(state, &read, &[]);
                    assert_eq!(eod.status, SCSI_STATUS_CHECK_CONDITION);
                    assert_eq!(eod.sense[2] & 0x0f, 0x08);

                    good(state, &[1, 0, 0, 0, 0, 0], &[]);
                    mode(state, selected_size);
                    good(state, &[0x11, 1, 0, 0, 4, 0], &[]);
                    assert_eq!(state.current_position, state.eod_position, "{case}");
                    let beyond = dispatch_raw_cdb(state, &[0x11, 1, 0, 0, 1, 0], &[]);
                    assert_eq!(beyond.status, SCSI_STATUS_CHECK_CONDITION);
                    assert_eq!(state.current_position, state.eod_position, "{case}");
                }
            }
        }
    }
}
