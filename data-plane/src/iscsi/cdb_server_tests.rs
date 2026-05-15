mod tests {
    use super::*;
    use crate::iscsi::cdb_changer::dispatch_changer_cdb;
    use crate::iscsi::cdb_drive::*;
    use std::fs;
    use std::io::{self, Cursor};
    use std::sync::Mutex;
    use std::time::{SystemTime, UNIX_EPOCH};

    fn temp_layout_for_test(case: &str) -> crate::storage::LayoutPaths {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let root = std::env::temp_dir()
            .join("holo-mam-tests")
            .join(format!("{case}-{nanos}"));
        fs::create_dir_all(&root).expect("create temp layout root");
        crate::storage::LayoutPaths {
            root: root.clone(),
            data_file: root.join("data.segment"),
            metadata_file: root.join("metadata.segment"),
            blk_map_file: root.join("blk_map.segment"),
            lookup_file: root.join("lookup.segment"),
            reclaim_file: root.join("reclaim.segment"),
            dedup_file: root.join("dedup.segment"),
            segment_index_file: root.join("segment_index.segment"),
        }
    }

    fn read_attr_entry(state: &mut crate::scsi_tape::state::TapeState, attr: u16) -> (u8, Vec<u8>) {
        let mut cdb = vec![
            0x8C, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // op/service + partition
            0x00, 0x00, // first attribute
            0x00, 0x00, 0x02, 0x00, // allocation length
            0x00, 0x00,
        ];
        cdb[8..10].copy_from_slice(&attr.to_be_bytes());
        let response = dispatch_raw_cdb(state, &cdb, &[]);
        assert_eq!(
            response.status, SCSI_STATUS_GOOD,
            "sense={:?}",
            response.sense
        );
        assert!(response.reply.len() >= 9);
        assert_eq!(&response.reply[4..6], &attr.to_be_bytes());
        let format = response.reply[6];
        let value_len = u16::from_be_bytes([response.reply[7], response.reply[8]]) as usize;
        assert!(response.reply.len() >= 9 + value_len);
        (format, response.reply[9..9 + value_len].to_vec())
    }

    fn read_attr_value(state: &mut crate::scsi_tape::state::TapeState, attr: u16) -> Vec<u8> {
        read_attr_entry(state, attr).1
    }

    fn attr_u64(state: &mut crate::scsi_tape::state::TapeState, attr: u16) -> u64 {
        let value = read_attr_value(state, attr);
        assert_eq!(value.len(), 8);
        u64::from_be_bytes(value.try_into().expect("u64 attr"))
    }

    fn drain_unit_attention(state: &mut crate::scsi_tape::state::TapeState) {
        while state.take_unit_attention().is_some() {}
    }

    #[test]
    fn cdb_cache_poison_lock_returns_io_error() {
        let mutex = Mutex::new(());
        let _ = std::panic::catch_unwind(|| {
            let _guard = mutex.lock().expect("test lock");
            panic!("poison test mutex");
        });

        let err = lock_io_mutex(&mutex, "media state")
            .expect_err("poisoned media state cache should return an error");

        assert_eq!(err.kind(), io::ErrorKind::Other);
        assert!(err.to_string().contains("media state cache poisoned"));
        mutex.clear_poison();
    }

    fn log_param_u32(page: &[u8], code: u16) -> Option<u32> {
        let mut cursor = 4usize;
        while page.len().saturating_sub(cursor) >= 4 {
            let param_code = u16::from_be_bytes([page[cursor], page[cursor + 1]]);
            let len = page[cursor + 3] as usize;
            cursor += 4;
            if page.len() < cursor + len {
                return None;
            }
            if param_code == code && len == 4 {
                return Some(u32::from_be_bytes([
                    page[cursor],
                    page[cursor + 1],
                    page[cursor + 2],
                    page[cursor + 3],
                ]));
            }
            cursor += len;
        }
        None
    }

    fn log_partition_param_u32(page: &[u8], code: u16, partition: u16) -> Option<u32> {
        let mut cursor = 4usize;
        while page.len().saturating_sub(cursor) >= 4 {
            let param_code = u16::from_be_bytes([page[cursor], page[cursor + 1]]);
            let len = page[cursor + 3] as usize;
            cursor += 4;
            if page.len() < cursor + len {
                return None;
            }
            if param_code == code {
                let mut record_cursor = cursor;
                while cursor + len >= record_cursor + 8 {
                    let record_len = page[record_cursor] as usize;
                    if record_len != 7 || cursor + len < record_cursor + 8 {
                        break;
                    }
                    let record_partition =
                        u16::from_be_bytes([page[record_cursor + 2], page[record_cursor + 3]]);
                    if record_partition == partition {
                        return Some(u32::from_be_bytes([
                            page[record_cursor + 4],
                            page[record_cursor + 5],
                            page[record_cursor + 6],
                            page[record_cursor + 7],
                        ]));
                    }
                    record_cursor += record_len + 1;
                }
            }
            cursor += len;
        }
        None
    }

    #[test]
    fn test_cdb_packet_encode_decode_roundtrip() {
        let cdb = vec![0x12, 0x00, 0x00, 0x00, 0x24, 0x00]; // INQUIRY
        let data = vec![0xDE, 0xAD, 0xBE, 0xEF];
        let pkt = CdbPacket::new(cdb.clone(), data.clone());
        let mut buf = Vec::new();
        pkt.encode(&mut buf).expect("encode failed");
        let mut cursor = Cursor::new(&buf);
        let decoded = CdbPacket::decode(&mut cursor).expect("decode failed");
        assert_eq!(decoded.cdb, cdb);
        assert_eq!(decoded.data, data);
        assert!(decoded.initiator.is_none());
    }

    #[test]
    fn test_cdb_packet_encode_decode_with_initiator_roundtrip() {
        let cdb = vec![0x5F, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x18, 0x00];
        let data = vec![0x00; 24];
        let pkt =
            CdbPacket::with_initiator(cdb.clone(), data.clone(), "iqn.1993-08.org.debian:host-a");
        let mut buf = Vec::new();
        pkt.encode(&mut buf).expect("encode failed");
        let mut cursor = Cursor::new(&buf);
        let decoded = CdbPacket::decode(&mut cursor).expect("decode failed");
        assert_eq!(decoded.cdb, cdb);
        assert_eq!(decoded.data, data);
        assert_eq!(
            decoded.initiator.as_deref(),
            Some("iqn.1993-08.org.debian:host-a")
        );
    }

    #[test]
    fn test_cdb_packet_decode_header_leaves_payload_for_reusable_buffer() {
        let cdb = vec![0x0A, 0x01, 0x00, 0x00, 0x01, 0x00]; // WRITE(6)
        let data = vec![0x5A; 4096];
        let pkt = CdbPacket::new(cdb.clone(), data.clone());
        let mut buf = Vec::new();
        pkt.encode(&mut buf).expect("encode failed");
        let mut cursor = Cursor::new(&buf);

        let header = CdbPacket::decode_header(&mut cursor).expect("header decode failed");
        assert_eq!(header.cdb, cdb);
        assert_eq!(header.data_len, data.len());
        assert!(header.initiator.is_none());

        let mut reusable = vec![0u8; header.data_len];
        std::io::Read::read_exact(&mut cursor, &mut reusable).expect("payload read failed");
        assert_eq!(reusable, data);
    }

    #[test]
    fn test_cdb_packet_encode_decode_no_data() {
        let cdb = vec![0x01, 0x00, 0x00, 0x00, 0x00, 0x00]; // REWIND
        let pkt = CdbPacket::new(cdb.clone(), vec![]);
        let mut buf = Vec::new();
        pkt.encode(&mut buf).expect("encode failed");
        let mut cursor = Cursor::new(&buf);
        let decoded = CdbPacket::decode(&mut cursor).expect("decode failed");
        assert_eq!(decoded.cdb, cdb);
        assert!(decoded.data.is_empty());
    }

    #[test]
    fn test_cdb_packet_decode_rejects_invalid_cdb_len() {
        let mut buf = Vec::new();
        buf.push(7u8);
        buf.extend_from_slice(&[0u8; 7]);
        buf.extend_from_slice(&0u32.to_be_bytes());
        let mut cursor = Cursor::new(&buf);
        let err = CdbPacket::decode(&mut cursor)
            .expect_err("decode should reject unsupported cdb length");
        assert_eq!(err.kind(), io::ErrorKind::InvalidData);
    }

    #[test]
    fn test_cdb_response_good_encode_decode_roundtrip() {
        let reply = vec![0x01, 0x80, 0x00, 0x00];
        let resp = CdbResponse::good(reply.clone());
        let mut buf = Vec::new();
        resp.encode(&mut buf).expect("encode failed");
        let mut cursor = Cursor::new(&buf);
        let decoded = CdbResponse::decode(&mut cursor).expect("decode failed");
        assert_eq!(decoded.status, SCSI_STATUS_GOOD);
        assert!(decoded.sense.is_empty());
        assert_eq!(decoded.reply, reply);
    }

    #[test]
    fn test_cdb_response_check_condition_encode_decode_roundtrip() {
        let sense = vec![
            0x70, 0x00, 0x05, 0x00, 0x00, 0x00, 0x00, 0x0A, 0x00, 0x00, 0x00, 0x00, 0x24, 0x00,
            0x00, 0x00, 0x00, 0x00,
        ];
        let resp = CdbResponse::check_condition(sense.clone());
        let mut buf = Vec::new();
        resp.encode(&mut buf).expect("encode failed");
        let mut cursor = Cursor::new(&buf);
        let decoded = CdbResponse::decode(&mut cursor).expect("decode failed");
        assert_eq!(decoded.status, SCSI_STATUS_CHECK_CONDITION);
        assert_eq!(decoded.sense, sense);
        assert!(decoded.reply.is_empty());
    }

    #[test]
    fn test_cdb_response_busy_encode_decode_roundtrip() {
        let resp = CdbResponse::busy();
        let mut buf = Vec::new();
        resp.encode(&mut buf).expect("encode failed");
        let mut cursor = Cursor::new(&buf);
        let decoded = CdbResponse::decode(&mut cursor).expect("decode failed");
        assert_eq!(decoded.status, SCSI_STATUS_BUSY);
        assert!(decoded.sense.is_empty());
        assert!(decoded.reply.is_empty());
    }

    #[test]
    fn test_request_sense_reports_power_on_unit_attention_once() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-ua-poweron");
        let request_sense = vec![0x03, 0x00, 0x00, 0x00, 0x12, 0x00];
        let first = dispatch_raw_cdb(&mut state, &request_sense, &[]);
        assert_eq!(first.status, SCSI_STATUS_GOOD);
        assert!(first.reply.len() >= 14);
        assert_eq!(first.reply[2] & 0x0F, 0x06);
        assert_eq!(first.reply[12], 0x29);
        assert_eq!(first.reply[13], 0x00);

        let second = dispatch_raw_cdb(&mut state, &request_sense, &[]);
        assert_eq!(second.status, SCSI_STATUS_GOOD);
        assert!(second.reply.len() >= 3);
        assert_eq!(second.reply[2] & 0x0F, 0x00);
    }

    #[test]
    fn test_unit_attention_queue_preserves_order() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-ua-order");
        // Drain initial power-on UA first.
        let request_sense = vec![0x03, 0x00, 0x00, 0x00, 0x12, 0x00];
        let _ = dispatch_raw_cdb(&mut state, &request_sense, &[]);

        state.push_unit_attention(0x28, 0x00);
        state.push_unit_attention(0x2A, 0x01);

        let first = dispatch_raw_cdb(&mut state, &request_sense, &[]);
        assert_eq!(first.reply[12], 0x28);
        assert_eq!(first.reply[13], 0x00);

        let second = dispatch_raw_cdb(&mut state, &request_sense, &[]);
        assert_eq!(second.reply[12], 0x2A);
        assert_eq!(second.reply[13], 0x01);
    }

    #[test]
    fn test_unit_attention_queue_is_bounded() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-ua-bounded");
        let _ = state.take_unit_attention();

        for idx in 0..20u8 {
            state.push_unit_attention(0x30 + idx, idx);
        }

        assert_eq!(state.pending_unit_attention.len(), 16);
        assert_eq!(state.take_unit_attention(), Some((0x34, 0x04)));
        assert_eq!(state.pending_unit_attention.len(), 15);
    }

    #[test]
    fn test_cdb_packet_decode_rejects_oversized_data() {
        let cdb_len: u8 = 6;
        let cdb = vec![0u8; 6];
        let oversized: u32 = (MAX_DATA_LEN + 1) as u32;
        let mut buf = vec![cdb_len];
        buf.extend_from_slice(&cdb);
        buf.extend_from_slice(&oversized.to_be_bytes());
        let mut cursor = Cursor::new(&buf);
        let result = CdbPacket::decode(&mut cursor);
        assert!(result.is_err(), "should reject oversized data_len");
    }

    #[test]
    fn test_dispatch_raw_cdb_rewind_not_ready() {
        // Without media loaded, REWIND should return CHECK CONDITION
        let mut state = crate::scsi_tape::state::TapeState::new("test-drive");
        let cdb = vec![0x01, 0x00, 0x00, 0x00, 0x00, 0x00]; // REWIND
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(response.status, SCSI_STATUS_CHECK_CONDITION);
        assert!(!response.sense.is_empty());
    }

    #[test]
    fn test_tur_reports_and_consumes_pending_unit_attention() {
        let mut state = crate::scsi_tape::state::TapeState::new("test-drive");
        let _ = state.take_unit_attention();
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        state.push_unit_attention(0x28, 0x00);
        let cdb = vec![0x00, 0x00, 0x00, 0x00, 0x00, 0x00];

        let first = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(first.status, SCSI_STATUS_CHECK_CONDITION);
        assert_eq!(first.sense[2] & 0x0F, 0x06);
        assert_eq!(first.sense[12], 0x28);
        assert_eq!(first.sense[13], 0x00);

        let second = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(second.status, SCSI_STATUS_GOOD);
    }

    #[test]
    fn test_tur_reports_unit_attention_before_not_ready() {
        let mut state = crate::scsi_tape::state::TapeState::new("test-drive");
        let _ = state.take_unit_attention();
        state.push_unit_attention(0x29, 0x00);
        let cdb = vec![0x00, 0x00, 0x00, 0x00, 0x00, 0x00];

        let first = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(first.status, SCSI_STATUS_CHECK_CONDITION);
        assert_eq!(first.sense[2] & 0x0F, 0x06);
        assert_eq!(first.sense[12], 0x29);

        let second = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(second.status, SCSI_STATUS_CHECK_CONDITION);
        assert_eq!(second.sense[2] & 0x0F, 0x02);
        assert_eq!(second.sense[12], 0x3A);
    }

    #[test]
    fn test_unit_attention_is_tracked_per_dispatch_initiator() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-ua-nexus");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        let cdb = vec![0x00, 0x00, 0x00, 0x00, 0x00, 0x00];

        let first_a = dispatch_raw_cdb_with_context(
            &mut state,
            &cdb,
            &[],
            CdbDispatchContext::with_initiator("host-a"),
        );
        assert_eq!(first_a.status, SCSI_STATUS_CHECK_CONDITION);
        assert_eq!(first_a.sense[2] & 0x0F, 0x06);
        assert_eq!(first_a.sense[12], 0x29);

        let second_a = dispatch_raw_cdb_with_context(
            &mut state,
            &cdb,
            &[],
            CdbDispatchContext::with_initiator("host-a"),
        );
        assert_eq!(second_a.status, SCSI_STATUS_GOOD);

        let first_b = dispatch_raw_cdb_with_context(
            &mut state,
            &cdb,
            &[],
            CdbDispatchContext::with_initiator("host-b"),
        );
        assert_eq!(first_b.status, SCSI_STATUS_CHECK_CONDITION);
        assert_eq!(first_b.sense[2] & 0x0F, 0x06);
        assert_eq!(first_b.sense[12], 0x29);
    }

    #[test]
    fn test_read_reports_pending_unit_attention_before_io() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-read-ua");
        let _ = state.take_unit_attention();
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        state.push_unit_attention(0x28, 0x00);

        let read_cdb = vec![0x08, 0x01, 0x00, 0x00, 0x01, 0x00];
        let response = dispatch_raw_cdb(&mut state, &read_cdb, &[]);

        assert_eq!(response.status, SCSI_STATUS_CHECK_CONDITION);
        assert!(response.sense.len() >= 14);
        assert_eq!(response.sense[2] & 0x0F, 0x06);
        assert_eq!(response.sense[12], 0x28);
        assert_eq!(response.sense[13], 0x00);
    }

    #[test]
    fn test_space_blocks_on_blank_media_reports_blank_check_eod() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-space-blank");
        let _ = state.take_unit_attention();
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;

        let cdb = vec![0x11, 0x00, 0x00, 0x00, 0x01, 0x00];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);

        assert_eq!(response.status, SCSI_STATUS_CHECK_CONDITION);
        assert!(response.sense.len() >= 14);
        assert_eq!(response.sense[2] & 0x0F, 0x08);
        assert_eq!(response.sense[12], 0x00);
        assert_eq!(response.sense[13], 0x05);
    }

    #[test]
    fn test_dispatch_raw_cdb_unknown_opcode_returns_check_condition() {
        let mut state = crate::scsi_tape::state::TapeState::new("test-drive");
        let cdb = vec![0xFF, 0x00, 0x00, 0x00, 0x00, 0x00]; // invalid opcode
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(response.status, SCSI_STATUS_CHECK_CONDITION);
    }

    #[test]
    fn test_dispatch_raw_cdb_empty_cdb_returns_check_condition() {
        let mut state = crate::scsi_tape::state::TapeState::new("test-drive");
        let response = dispatch_raw_cdb(&mut state, &[], &[]);
        assert_eq!(response.status, SCSI_STATUS_CHECK_CONDITION);
    }

    #[test]
    fn test_dispatch_raw_cdb_rejects_invalid_cdb_length() {
        let mut state = crate::scsi_tape::state::TapeState::new("test-drive");
        let cdb = vec![0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00]; // REWIND with illegal length
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(response.status, SCSI_STATUS_CHECK_CONDITION);
        assert!(response.sense.len() >= 14);
        assert_eq!(response.sense[2] & 0x0F, 0x05);
        assert_eq!(response.sense[12], 0x24);
    }

    #[test]
    fn test_big_endian_field_readers_reject_short_buffers() {
        assert_eq!(try_read_be16(&[0x12], 0), None);
        assert_eq!(try_read_be24(&[0x12, 0x34], 0), None);
        assert_eq!(try_read_be32(&[0x12, 0x34, 0x56], 0), None);
        assert_eq!(try_read_be16(&[0x12, 0x34], 0), Some(0x1234));
        assert_eq!(try_read_be24(&[0x12, 0x34, 0x56], 0), Some(0x123456));
        assert_eq!(
            try_read_be32(&[0x12, 0x34, 0x56, 0x78], 0),
            Some(0x12345678)
        );
    }

    #[test]
    fn test_sense_frame_to_bytes_fixed_format() {
        use crate::scsi_tape::sense::{sense_for_context, SenseContext};
        let frame = sense_for_context(SenseContext::NotReady);
        let bytes = sense_frame_to_bytes(&frame);
        assert_eq!(bytes.len(), 18);
        assert_eq!(bytes[0], 0x70); // fixed format, current errors
        assert_eq!(bytes[2], 0x02); // sense key NOT READY
        assert_eq!(bytes[12], 0x3A); // ASC
        assert_eq!(bytes[13], 0x00); // ASCQ
    }

    #[test]
    fn test_sense_frame_blank_check_sets_valid_info_field() {
        use crate::scsi_tape::sense::{sense_for_context, SenseContext};
        let frame = sense_for_context(SenseContext::BlankCheckEod);
        let bytes = sense_frame_to_bytes(&frame);
        assert_eq!(bytes[0], 0xF0); // fixed format + VALID bit
        assert_eq!(&bytes[3..7], &1u32.to_be_bytes());
        assert_eq!(bytes[2], 0x08); // blank check
        assert_eq!(bytes[12], 0x00);
        assert_eq!(bytes[13], 0x05);
    }

    #[test]
    fn test_sense_frame_volume_overflow_sets_eom_bit() {
        let frame = crate::scsi_tape::commands_core::tape_error_to_sense(
            &crate::scsi_tape::error::TapeError::VolumeOverflow,
        );
        let bytes = sense_frame_to_bytes(&frame);
        assert_eq!(bytes[2] & 0x0F, 0x0D);
        assert_eq!(bytes[2] & 0x40, 0x40);
        assert_eq!(bytes[12], 0x00);
        assert_eq!(bytes[13], 0x00);
    }

    #[test]
    fn test_inquiry_vpd_serial_uses_drive_seed() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-abc");
        let cdb = vec![0x12, 0x01, 0x80, 0x00, 0xFF, 0x00]; // EVPD page 0x80
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);

        assert_eq!(
            response.status, SCSI_STATUS_GOOD,
            "sense={:?}",
            response.sense
        );
        assert!(response.reply.len() >= 4);
        assert_eq!(response.reply[1], 0x80);

        let serial = String::from_utf8_lossy(&response.reply[4..]).to_string();
        assert!(
            serial.to_ascii_lowercase().contains("driveabc"),
            "serial should include sanitized drive seed, got: {serial}"
        );
    }

    #[test]
    fn test_inquiry_standard_returns_changer_when_role_is_changer() {
        let mut state = crate::scsi_tape::state::TapeState::new("changer-1");
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");
        let cdb = vec![0x12, 0x00, 0x00, 0x00, 0xFF, 0x00];
        let response = dispatch_changer_cdb(&mut state, &cdb, &[], profile);

        assert_eq!(
            response.status, SCSI_STATUS_GOOD,
            "sense={:?}",
            response.sense
        );
        assert!(response.reply.len() >= 36);
        assert_eq!(response.reply[0] & 0x1F, 0x08, "expected PDT=8 changer");
        let vendor = String::from_utf8_lossy(&response.reply[8..16])
            .trim_end()
            .to_string();
        assert_eq!(vendor, "IBM");
    }

    #[test]
    fn test_test_unit_ready_returns_not_ready_when_no_media() {
        let mut state = crate::scsi_tape::state::TapeState::new("tur");
        while state.take_unit_attention().is_some() {}
        let cdb = vec![0x00, 0x00, 0x00, 0x00, 0x00, 0x00];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(response.status, SCSI_STATUS_CHECK_CONDITION);
        assert_eq!(response.sense.get(2).copied().unwrap_or(0) & 0x0F, 0x02);
    }

    #[test]
    fn test_test_unit_ready_returns_good_when_loaded() {
        let mut state = crate::scsi_tape::state::TapeState::new("tur-loaded");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        while state.take_unit_attention().is_some() {}

        let cdb = vec![0x00, 0x00, 0x00, 0x00, 0x00, 0x00];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(
            response.status, SCSI_STATUS_GOOD,
            "write6 multi-block sense={:?}",
            response.sense
        );
        assert!(response.reply.is_empty());
    }

    #[test]
    fn test_changer_mode_sense6_returns_pages() {
        let mut state = crate::scsi_tape::state::TapeState::new("changer-ms");
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");
        let cdb = vec![0x1A, 0x00, 0x3F, 0x00, 0x60, 0x00];
        let response = dispatch_changer_cdb(&mut state, &cdb, &[], profile);

        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert!(response.reply.len() >= 4);
        assert_eq!(
            response.reply[0] as usize,
            response.reply.len().saturating_sub(1)
        );
        assert_eq!(response.reply[4], 0x1D);
    }

    #[test]
    fn test_changer_mode_sense10_returns_pages() {
        let mut state = crate::scsi_tape::state::TapeState::new("changer-ms10");
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");
        let cdb = vec![0x5A, 0x00, 0x1D, 0x00, 0x00, 0x00, 0x00, 0x00, 0x40, 0x00];
        let response = dispatch_changer_cdb(&mut state, &cdb, &[], profile);

        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert!(response.reply.len() >= 8);
        assert_eq!(response.reply[8], 0x1D);
    }

    #[test]
    fn test_changer_read_element_status_returns_descriptor_payload() {
        let mut state = crate::scsi_tape::state::TapeState::new("changer-res");
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");
        let cdb = vec![
            0xB8, 0x00, // opcode, element type
            0x01, 0x00, // start element address = 0x0100
            0x00, 0x02, // number of elements requested = 2
            0x00, 0x00, 0x00, 0x40, // allocation length
            0x00, 0x00, // flags/control
        ];
        let response = dispatch_changer_cdb(&mut state, &cdb, &[], profile);

        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert!(response.reply.len() >= 24);
        let element_count = u16::from_be_bytes([response.reply[2], response.reply[3]]);
        assert!(element_count >= 1);
        // First descriptor page type should be one of MT/ST/DT for ALL TYPES.
        assert!(matches!(response.reply[8], 0x01 | 0x02 | 0x04));
    }

    #[test]
    fn test_changer_read_element_status_drive_with_dvcid_returns_identifier() {
        let mut state = crate::scsi_tape::state::TapeState::new("changer-res-dvcid");
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");
        let cdb = vec![
            0xB8, 0x04, // DATA TRANSFER ELEMENT
            0x01, 0x00, // start = 0x0100
            0x00, 0x01, // one element
            0x01, // DVCID=1
            0x00, 0x01, 0x00, // alloc = 256
            0x00, 0x00,
        ];
        let response = dispatch_changer_cdb(&mut state, &cdb, &[], profile);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert!(response.reply.len() >= 32);

        // page header starts at byte 8
        assert_eq!(response.reply[8], 0x04);
        let descriptor_len = u16::from_be_bytes([response.reply[10], response.reply[11]]) as usize;
        assert!(
            descriptor_len > 12,
            "descriptor length should include identifier payload"
        );

        // first descriptor starts at byte 16; identifier starts after common descriptor (12 bytes).
        let identifier_offset = 16 + 12;
        assert_eq!(response.reply[identifier_offset], 0x02); // ASCII
        assert_eq!(response.reply[identifier_offset + 1], 0x01); // T10 vendor identifier
        assert!(response.reply[identifier_offset + 3] >= 24); // vendor+product+serial
    }

    #[test]
    fn test_changer_read_element_status_drive_with_avoltag_expands_descriptor() {
        let mut state = crate::scsi_tape::state::TapeState::new("changer-res-avoltag");
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("hp-eml-e-series");
        let cdb = vec![
            0xB8, 0x34, // DATA TRANSFER ELEMENT + VolTag + AVolTag
            0x01, 0x00, // start = 0x0100
            0x00, 0x01, // one element
            0x00, // no DVCID
            0x00, 0x01, 0x00, // alloc = 256
            0x00, 0x00,
        ];
        let response = dispatch_changer_cdb(&mut state, &cdb, &[], profile);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert!(response.reply.len() >= 104);
        assert_eq!(response.reply[8], 0x04);
        assert_eq!(response.reply[9] & 0xC0, 0xC0);
        let descriptor_len = u16::from_be_bytes([response.reply[10], response.reply[11]]) as usize;
        assert_eq!(descriptor_len, 88);

        // alternate voltag starts after common descriptor(12) + primary voltag(36)
        let avoltag_offset = 16 + 12 + 36;
        assert_ne!(response.reply[avoltag_offset + 4], b' ');
    }

    #[test]
    fn test_changer_read_element_status_primary_voltag_has_no_leading_spaces() {
        let mut state = crate::scsi_tape::state::TapeState::new("changer-res-primary-voltag");
        state
            .changer_slots
            .insert(0x0400, Some("VTA000L06".to_string()));
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");
        let cdb = vec![
            0xB8, 0x12, // STORAGE ELEMENT + VolTag
            0x04, 0x00, // start = 0x0400
            0x00, 0x01, // one element
            0x00, 0x00, 0x00, 0x80, // alloc = 128
            0x00, 0x00,
        ];
        let response = dispatch_changer_cdb(&mut state, &cdb, &[], profile);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert!(response.reply.len() >= 64);
        assert_eq!(response.reply[8], 0x02); // storage element page
        let descriptor_len = u16::from_be_bytes([response.reply[10], response.reply[11]]) as usize;
        assert_eq!(descriptor_len, 52); // common(12) + voltag(36) + identifier(4)

        let primary_voltag_offset = 16 + 12;
        assert_eq!(
            &response.reply[primary_voltag_offset..primary_voltag_offset + 9],
            b"VTA000L06"
        );
        assert_ne!(response.reply[primary_voltag_offset], b' ');
        assert_eq!(
            &response.reply[primary_voltag_offset + 32..primary_voltag_offset + 36],
            &[0x00, 0x00, 0x00, 0x00]
        );
    }

    #[test]
    fn test_changer_read_element_status_drive_sets_svalid_source_address() {
        let mut state = crate::scsi_tape::state::TapeState::new("changer-res-source");
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");
        let move_cdb = vec![
            0xA5, 0x00, // MOVE MEDIUM
            0x00, 0x00, // transport address
            0x04, 0x00, // source slot
            0x01, 0x00, // destination drive
            0x00, 0x00, 0x00, 0x00,
        ];
        let move_response = dispatch_changer_cdb(&mut state, &move_cdb, &[], profile.clone());
        assert_eq!(move_response.status, SCSI_STATUS_GOOD);

        let cdb = vec![
            0xB8, 0x04, // DATA TRANSFER ELEMENT
            0x01, 0x00, // start = 0x0100
            0x00, 0x01, // count = 1
            0x00, 0x00, 0x00, 0x80, // alloc
            0x00, 0x00,
        ];
        let response = dispatch_changer_cdb(&mut state, &cdb, &[], profile);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert!(response.reply.len() >= 30);

        let descriptor_offset = 16usize;
        let invert = response.reply[descriptor_offset + 9];
        let source = u16::from_be_bytes([
            response.reply[descriptor_offset + 10],
            response.reply[descriptor_offset + 11],
        ]);
        assert_eq!(
            invert & 0x80,
            0x80,
            "SVALID should be set for drive element"
        );
        assert_eq!(
            source, 0x0400,
            "source storage element should point to slot range"
        );
    }

    #[test]
    fn test_changer_move_medium_updates_slot_and_drive_fullness() {
        let mut state = crate::scsi_tape::state::TapeState::new("changer-move");
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");
        let move_cdb = vec![
            0xA5, 0x00, // MOVE MEDIUM
            0x00, 0x00, // transport address
            0x04, 0x00, // source slot 0x0400
            0x01, 0x00, // destination drive 0x0100
            0x00, 0x00, 0x00, 0x00,
        ];
        let move_response = dispatch_changer_cdb(&mut state, &move_cdb, &[], profile.clone());
        assert_eq!(move_response.status, SCSI_STATUS_GOOD);

        // Slot becomes empty.
        let slot_status_cdb = vec![
            0xB8, 0x02, // STORAGE ELEMENT
            0x04, 0x00, // start 0x0400
            0x00, 0x01, // one element
            0x00, 0x00, 0x00, 0x80, 0x00, 0x00,
        ];
        let slot_status = dispatch_changer_cdb(&mut state, &slot_status_cdb, &[], profile.clone());
        assert_eq!(slot_status.status, SCSI_STATUS_GOOD);
        let slot_descriptor_offset = 16usize;
        assert_eq!(
            slot_status.reply[slot_descriptor_offset + 2] & 0x01,
            0x00,
            "slot should not report FULL after MOVE MEDIUM"
        );

        // Drive becomes full and points back to source slot.
        let drive_status_cdb = vec![
            0xB8, 0x04, // DATA TRANSFER ELEMENT
            0x01, 0x00, // start 0x0100
            0x00, 0x01, // one element
            0x00, 0x00, 0x00, 0x80, 0x00, 0x00,
        ];
        let drive_status = dispatch_changer_cdb(&mut state, &drive_status_cdb, &[], profile);
        assert_eq!(drive_status.status, SCSI_STATUS_GOOD);
        let drive_descriptor_offset = 16usize;
        assert_eq!(
            drive_status.reply[drive_descriptor_offset + 2] & 0x01,
            0x01,
            "drive should report FULL after MOVE MEDIUM"
        );
        let source = u16::from_be_bytes([
            drive_status.reply[drive_descriptor_offset + 10],
            drive_status.reply[drive_descriptor_offset + 11],
        ]);
        assert_eq!(source, 0x0400);
    }

    #[test]
    fn test_changer_move_medium_does_not_rehydrate_slot_from_shared_between_commands() {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let serial = format!("changer-no-rehydrate-{nanos}");
        let mut state = crate::scsi_tape::state::TapeState::new(serial.clone());
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");
        write_shared_changer_slots(
            &serial,
            &[
                Some("VTL000001".to_string()),
                Some("VTL000002".to_string()),
                None,
            ],
        )
        .expect("write shared slots");

        let init_cdb = vec![0x07, 0x00, 0x00, 0x00, 0x00, 0x00];
        let init_response = dispatch_changer_cdb(&mut state, &init_cdb, &[], profile.clone());
        assert_eq!(init_response.status, SCSI_STATUS_GOOD);

        let move_cdb = vec![
            0xA5, 0x00, // MOVE MEDIUM
            0x00, 0x00, // transport address
            0x04, 0x00, // source slot 0x0400
            0x01, 0x00, // destination drive 0x0100
            0x00, 0x00, 0x00, 0x00,
        ];
        let move_response = dispatch_changer_cdb(&mut state, &move_cdb, &[], profile.clone());
        assert_eq!(move_response.status, SCSI_STATUS_GOOD);

        let slot_status_cdb = vec![
            0xB8, 0x02, // STORAGE ELEMENT
            0x04, 0x00, // start 0x0400
            0x00, 0x01, // one element
            0x00, 0x00, 0x00, 0x80, 0x00, 0x00,
        ];
        let first = dispatch_changer_cdb(&mut state, &slot_status_cdb, &[], profile.clone());
        assert_eq!(first.status, SCSI_STATUS_GOOD);
        assert_eq!(
            first.reply[16 + 2] & 0x01,
            0x00,
            "slot should stay empty after move"
        );

        let second = dispatch_changer_cdb(&mut state, &slot_status_cdb, &[], profile);
        assert_eq!(second.status, SCSI_STATUS_GOOD);
        assert_eq!(
            second.reply[16 + 2] & 0x01,
            0x00,
            "slot should remain empty across subsequent changer commands"
        );

        let _ = std::fs::remove_file(shared_changer_slots_path(&serial));
        let _ = std::fs::remove_file(shared_media_state_path(&serial));
    }

    #[test]
    fn test_changer_move_medium_bootstraps_shared_slots_without_initialize() {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let serial = format!("changer-bootstrap-move-{nanos}");
        let mut state = crate::scsi_tape::state::TapeState::new(serial.clone());
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");
        write_shared_changer_slots(
            &serial,
            &[
                Some("VTA000L06".to_string()),
                Some("VTA001L06".to_string()),
                None,
            ],
        )
        .expect("write shared slots");

        let move_cdb = vec![
            0xA5, 0x00, // MOVE MEDIUM
            0x00, 0x00, // transport address
            0x04, 0x00, // source slot 0x0400
            0x01, 0x00, // destination drive 0x0100
            0x00, 0x00, 0x00, 0x00,
        ];
        let move_response = dispatch_changer_cdb(&mut state, &move_cdb, &[], profile);
        assert_eq!(move_response.status, SCSI_STATUS_GOOD);
        assert_eq!(
            state.changer_drives.get(&0x0100).and_then(|v| v.as_deref()),
            Some("VTA000L06")
        );
        assert_eq!(
            state.changer_slots.get(&0x0400).and_then(|v| v.as_deref()),
            None
        );
        assert_eq!(
            read_shared_loaded_cartridge(&serial)
                .expect("read shared loaded cartridge")
                .as_deref(),
            Some("VTA000L06")
        );

        let _ = std::fs::remove_file(shared_changer_slots_path(&serial));
        let _ = std::fs::remove_file(shared_media_state_path(&serial));
    }

    #[test]
    fn test_changer_move_medium_to_ie_sets_ie_full_and_reports_ie_accessed_ua() {
        let mut state = crate::scsi_tape::state::TapeState::new("changer-ie-move");
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");
        // Drain initial power-on UA (0x29/0x00).
        let sense_cdb = vec![0x03, 0x00, 0x00, 0x00, 0x12, 0x00];
        let _ = dispatch_changer_cdb(&mut state, &sense_cdb, &[], profile.clone());

        let move_to_ie_cdb = vec![
            0xA5, 0x00, // MOVE MEDIUM
            0x00, 0x00, // transport address
            0x04, 0x00, // source slot 0x0400
            0x03, 0x00, // destination IE 0x0300
            0x00, 0x00, 0x00, 0x00,
        ];
        let move_response = dispatch_changer_cdb(&mut state, &move_to_ie_cdb, &[], profile.clone());
        assert_eq!(move_response.status, SCSI_STATUS_GOOD);

        let ie_status_cdb = vec![
            0xB8, 0x03, // IMPORT/EXPORT ELEMENT
            0x03, 0x00, // start 0x0300
            0x00, 0x01, // one element
            0x00, 0x00, 0x00, 0x80, 0x00, 0x00,
        ];
        let ie_status = dispatch_changer_cdb(&mut state, &ie_status_cdb, &[], profile.clone());
        assert_eq!(ie_status.status, SCSI_STATUS_GOOD);
        let ie_descriptor_offset = 16usize;
        assert_eq!(
            ie_status.reply[ie_descriptor_offset + 2] & 0x01,
            0x01,
            "ie element should report FULL after MOVE MEDIUM"
        );
        assert_eq!(
            ie_status.reply[ie_descriptor_offset + 2] & 0x02,
            0x02,
            "ie element should carry IMPEXP bit after IE load"
        );

        let sense_response = dispatch_changer_cdb(&mut state, &sense_cdb, &[], profile);
        assert_eq!(sense_response.status, SCSI_STATUS_GOOD);
        assert!(sense_response.reply.len() >= 14);
        assert_eq!(sense_response.reply[2] & 0x0F, 0x06); // UNIT ATTENTION
        assert_eq!(sense_response.reply[12], 0x28); // import/export element accessed
        assert_eq!(sense_response.reply[13], 0x01);
    }

    #[test]
    fn test_changer_read_ie_element_status_reports_ie_accessed_ua() {
        let mut state = crate::scsi_tape::state::TapeState::new("changer-ie-status-ua");
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");
        // Drain initial power-on UA (0x29/0x00).
        let sense_cdb = vec![0x03, 0x00, 0x00, 0x00, 0x12, 0x00];
        let _ = dispatch_changer_cdb(&mut state, &sense_cdb, &[], profile.clone());

        let ie_status_cdb = vec![
            0xB8, 0x03, // IMPORT/EXPORT ELEMENT
            0x03, 0x00, // start = 0x0300
            0x00, 0x01, // count = 1
            0x00, 0x00, 0x00, 0x80, // alloc
            0x00, 0x00,
        ];
        let response = dispatch_changer_cdb(&mut state, &ie_status_cdb, &[], profile.clone());
        assert_eq!(response.status, SCSI_STATUS_GOOD);

        let sense_response = dispatch_changer_cdb(&mut state, &sense_cdb, &[], profile);
        assert_eq!(sense_response.status, SCSI_STATUS_GOOD);
        assert!(sense_response.reply.len() >= 14);
        assert_eq!(sense_response.reply[2] & 0x0F, 0x06);
        assert_eq!(sense_response.reply[12], 0x28);
        assert_eq!(sense_response.reply[13], 0x01);
    }

    #[test]
    fn test_changer_ie_impexp_flag_clears_after_unload_from_ie() {
        let mut state = crate::scsi_tape::state::TapeState::new("changer-ie-flag-clear");
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");
        // Load slot -> IE.
        let load_ie = vec![
            0xA5, 0x00, 0x00, 0x00, 0x04, 0x00, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00,
        ];
        let response = dispatch_changer_cdb(&mut state, &load_ie, &[], profile.clone());
        assert_eq!(response.status, SCSI_STATUS_GOOD);

        // Unload IE -> empty slot.
        let unload_ie = vec![
            0xA5, 0x00, 0x00, 0x00, 0x03, 0x00, 0x04, 0x0A, 0x00, 0x00, 0x00, 0x00,
        ];
        let response = dispatch_changer_cdb(&mut state, &unload_ie, &[], profile.clone());
        assert_eq!(response.status, SCSI_STATUS_GOOD);

        let ie_status_cdb = vec![
            0xB8, 0x03, // IMPORT/EXPORT ELEMENT
            0x03, 0x00, // start = 0x0300
            0x00, 0x01, // count = 1
            0x00, 0x00, 0x00, 0x80, 0x00, 0x00,
        ];
        let status = dispatch_changer_cdb(&mut state, &ie_status_cdb, &[], profile);
        assert_eq!(status.status, SCSI_STATUS_GOOD);
        let offset = 16usize;
        assert_eq!(status.reply[offset + 2] & 0x01, 0x00); // FULL cleared
        assert_eq!(status.reply[offset + 2] & 0x02, 0x00); // IMPEXP cleared
    }

    #[test]
    fn test_changer_move_medium_updates_shared_mount_state() {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let serial = format!("changer-shared-{nanos}");
        let mut state = crate::scsi_tape::state::TapeState::new(serial.clone());
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");
        let state_path = shared_media_state_path(&serial);
        let _ = std::fs::remove_file(&state_path);

        let load_to_drive = vec![
            0xA5, 0x00, // MOVE MEDIUM
            0x00, 0x00, // transport
            0x04, 0x00, // source slot
            0x01, 0x00, // destination drive
            0x00, 0x00, 0x00, 0x00,
        ];
        let response = dispatch_changer_cdb(&mut state, &load_to_drive, &[], profile.clone());
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        let shared = read_shared_loaded_cartridge(&serial).expect("read shared");
        assert_eq!(shared.as_deref(), Some("VTL000001"));

        let unload_to_empty_slot = vec![
            0xA5, 0x00, // MOVE MEDIUM
            0x00, 0x00, // transport
            0x01, 0x00, // source drive
            0x04, 0x0A, // destination slot (default empty)
            0x00, 0x00, 0x00, 0x00,
        ];
        let response = dispatch_changer_cdb(&mut state, &unload_to_empty_slot, &[], profile);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        let shared = read_shared_loaded_cartridge(&serial).expect("read shared");
        assert!(
            shared.is_none(),
            "shared state should clear after unload to slot"
        );
    }

    #[test]
    fn test_changer_unload_to_same_label_slot_repairs_duplicate_state() {
        let mut state = crate::scsi_tape::state::TapeState::new("changer-duplicate-unload");
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");
        state
            .changer_slots
            .insert(0x040E, Some("VTA014L06".to_string()));
        state
            .changer_drives
            .insert(0x0100, Some("VTA014L06".to_string()));
        state.changer_drive_sources.insert(0x0100, Some(0x040E));

        let unload_to_original_slot = vec![
            0xA5, 0x00, // MOVE MEDIUM
            0x00, 0x00, // transport
            0x01, 0x00, // source drive
            0x04, 0x0E, // destination slot
            0x00, 0x00, 0x00, 0x00,
        ];
        let response = dispatch_changer_cdb(&mut state, &unload_to_original_slot, &[], profile);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert_eq!(
            state.changer_drives.get(&0x0100).and_then(|v| v.as_deref()),
            None
        );
        assert_eq!(
            state.changer_slots.get(&0x040E).and_then(|v| v.as_deref()),
            Some("VTA014L06")
        );
    }

    #[test]
    fn test_changer_multi_drive_syncs_mount_state_per_drive_key() {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let library = format!("lib-multi-{nanos}");
        let key_a = format!("{library}__drivea");
        let key_b = format!("{library}__driveb");
        let mut state = crate::scsi_tape::state::TapeState::new(key_a.clone());
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");

        let slots = [
            Some("VTL000001".to_string()),
            Some("VTL000002".to_string()),
            None,
        ];
        write_shared_changer_slots(&key_a, &slots).expect("write slots for drive a");
        write_shared_changer_slots(&key_b, &slots).expect("write slots for drive b");
        let _ = std::fs::remove_file(shared_media_state_path(&key_a));
        let _ = std::fs::remove_file(shared_media_state_path(&key_b));

        let init_cdb = vec![0x07, 0x00, 0x00, 0x00, 0x00, 0x00];
        let init_response = dispatch_changer_cdb(&mut state, &init_cdb, &[], profile.clone());
        assert_eq!(init_response.status, SCSI_STATUS_GOOD);

        let drive_status_cdb = vec![
            0xB8, 0x04, // element type = drive
            0x01, 0x00, // start = 0x0100
            0x00, 0x02, // count = 2
            0x00, 0x00, 0x00, 0x80, 0x00, 0x00,
        ];
        let drive_status =
            dispatch_changer_cdb(&mut state, &drive_status_cdb, &[], profile.clone());
        assert_eq!(drive_status.status, SCSI_STATUS_GOOD);
        assert_eq!(
            u16::from_be_bytes([drive_status.reply[2], drive_status.reply[3]]),
            2
        );

        let load_second_drive = vec![
            0xA5, 0x00, // MOVE MEDIUM
            0x00, 0x00, // transport
            0x04, 0x00, // source slot
            0x01, 0x01, // destination drive (second)
            0x00, 0x00, 0x00, 0x00,
        ];
        let load_response =
            dispatch_changer_cdb(&mut state, &load_second_drive, &[], profile.clone());
        assert_eq!(load_response.status, SCSI_STATUS_GOOD);
        let shared_a = read_shared_loaded_cartridge(&key_a).expect("read shared a after load");
        let shared_b = read_shared_loaded_cartridge(&key_b).expect("read shared b after load");
        assert_eq!(shared_a, None);
        assert_eq!(shared_b.as_deref(), Some("VTL000001"));

        let unload_second_drive = vec![
            0xA5, 0x00, // MOVE MEDIUM
            0x00, 0x00, // transport
            0x01, 0x01, // source drive (second)
            0x04, 0x02, // destination slot within shared inventory range
            0x00, 0x00, 0x00, 0x00,
        ];
        let unload_response = dispatch_changer_cdb(&mut state, &unload_second_drive, &[], profile);
        assert_eq!(unload_response.status, SCSI_STATUS_GOOD);
        let shared_a = read_shared_loaded_cartridge(&key_a).expect("read shared a after unload");
        let shared_b = read_shared_loaded_cartridge(&key_b).expect("read shared b after unload");
        assert_eq!(shared_a, None);
        assert_eq!(shared_b, None);

        let _ = std::fs::remove_file(shared_changer_slots_path(&key_a));
        let _ = std::fs::remove_file(shared_changer_slots_path(&key_b));
        let _ = std::fs::remove_file(shared_media_state_path(&key_a));
        let _ = std::fs::remove_file(shared_media_state_path(&key_b));
    }

    #[test]
    fn test_changer_reads_shared_mount_state_into_drive_element() {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let library = format!("lib-shared-load-{nanos}");
        let key = format!("{library}__drivea");
        let mut state = crate::scsi_tape::state::TapeState::new(key.clone());
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");

        write_shared_changer_slots(
            &key,
            &[
                Some("VTL000001".to_string()),
                Some("VTL000002".to_string()),
                None,
            ],
        )
        .expect("write slots");
        write_shared_loaded_cartridge(&key, Some("VTL000001")).expect("write loaded state");

        let init_cdb = vec![0x07, 0x00, 0x00, 0x00, 0x00, 0x00];
        let init_response = dispatch_changer_cdb(&mut state, &init_cdb, &[], profile.clone());
        assert_eq!(init_response.status, SCSI_STATUS_GOOD);
        assert_eq!(
            state.changer_drives.get(&0x0100).and_then(|v| v.as_deref()),
            Some("VTL000001")
        );
        assert_eq!(
            state.changer_slots.get(&0x0400).and_then(|v| v.as_deref()),
            None
        );

        write_shared_loaded_cartridge(&key, None).expect("clear loaded state");
        let drive_status_cdb = vec![
            0xB8, 0x04, // element type = drive
            0x01, 0x00, // start = 0x0100
            0x00, 0x01, // count = 1
            0x00, 0x00, 0x00, 0x80, 0x00, 0x00,
        ];
        let status = dispatch_changer_cdb(&mut state, &drive_status_cdb, &[], profile);
        assert_eq!(status.status, SCSI_STATUS_GOOD);
        assert_eq!(
            state.changer_drives.get(&0x0100).and_then(|v| v.as_deref()),
            None
        );
        assert_eq!(
            state.changer_slots.get(&0x0400).and_then(|v| v.as_deref()),
            Some("VTL000001")
        );

        let _ = std::fs::remove_file(shared_changer_slots_path(&key));
        let _ = std::fs::remove_file(shared_media_state_path(&key));
    }

    #[test]
    fn test_changer_shared_drive_switch_restores_previous_medium_to_slot() {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let library = format!("lib-shared-switch-{nanos}");
        let key = format!("{library}__drivea");
        let mut state = crate::scsi_tape::state::TapeState::new(key.clone());
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");

        write_shared_changer_slots(
            &key,
            &[
                Some("VTA000L05".to_string()),
                Some("VTA001L05".to_string()),
                None,
            ],
        )
        .expect("write slots");
        write_shared_loaded_cartridge(&key, Some("VTA000L05")).expect("write initial loaded state");

        let init_cdb = vec![0x07, 0x00, 0x00, 0x00, 0x00, 0x00];
        let init_response = dispatch_changer_cdb(&mut state, &init_cdb, &[], profile.clone());
        assert_eq!(init_response.status, SCSI_STATUS_GOOD);
        assert_eq!(
            state.changer_drives.get(&0x0100).and_then(|v| v.as_deref()),
            Some("VTA000L05")
        );
        assert_eq!(
            state.changer_slots.get(&0x0400).and_then(|v| v.as_deref()),
            None
        );

        write_shared_changer_slots(&key, &[Some("VTA000L05".to_string()), None, None])
            .expect("write switched slots");
        write_shared_loaded_cartridge(&key, Some("VTA001L05"))
            .expect("write switched loaded state");

        let drive_status_cdb = vec![
            0xB8, 0x04, // element type = drive
            0x01, 0x00, // start = 0x0100
            0x00, 0x01, // count = 1
            0x00, 0x00, 0x00, 0x80, 0x00, 0x00,
        ];
        let status = dispatch_changer_cdb(&mut state, &drive_status_cdb, &[], profile);
        assert_eq!(status.status, SCSI_STATUS_GOOD);
        assert_eq!(
            state.changer_drives.get(&0x0100).and_then(|v| v.as_deref()),
            Some("VTA001L05")
        );
        assert_eq!(
            state.changer_slots.get(&0x0400).and_then(|v| v.as_deref()),
            Some("VTA000L05")
        );

        let _ = std::fs::remove_file(shared_changer_slots_path(&key));
        let _ = std::fs::remove_file(shared_media_state_path(&key));
        let _ = std::fs::remove_file(shared_changer_ie_path(&key));
    }

    #[test]
    fn test_changer_reads_shared_ie_export_state() {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let library = format!("lib-shared-ie-{nanos}");
        let key = format!("{library}__drivea");
        let mut state = crate::scsi_tape::state::TapeState::new(key.clone());
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");

        write_shared_changer_slots(&key, &[Some("VTA000L05".to_string()), None, None])
            .expect("write slots");
        write_shared_changer_ie(&key, &[Some("VTA009L05".to_string())]).expect("write IE");

        let ie_status_cdb = vec![
            0xB8, 0x03, // IMPORT/EXPORT ELEMENT
            0x03, 0x00, // start = 0x0300
            0x00, 0x01, // count = 1
            0x00, 0x00, 0x00, 0x80, 0x00, 0x00,
        ];
        let status = dispatch_changer_cdb(&mut state, &ie_status_cdb, &[], profile);
        assert_eq!(status.status, SCSI_STATUS_GOOD);
        assert_eq!(
            state
                .changer_ie_ports
                .get(&0x0300)
                .and_then(|v| v.as_deref()),
            Some("VTA009L05")
        );
        assert_eq!(status.reply[16 + 2] & 0x01, 0x01);
        assert_eq!(status.reply[16 + 2] & 0x02, 0x02);

        let _ = std::fs::remove_file(shared_changer_slots_path(&key));
        let _ = std::fs::remove_file(shared_media_state_path(&key));
        let _ = std::fs::remove_file(shared_changer_ie_path(&key));
    }

    #[test]
    fn test_changer_reads_shared_ie_count_from_shared_state() {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let library = format!("lib-shared-ie-count-{nanos}");
        let key = format!("{library}__drivea");
        let mut state = crate::scsi_tape::state::TapeState::new(key.clone());
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");

        write_shared_changer_ie(&key, &[None, None, None, None]).expect("write IE count");

        let ie_status_cdb = vec![
            0xB8, 0x03, // IMPORT/EXPORT ELEMENT
            0x03, 0x00, // start = 0x0300
            0x00, 0x04, // count = 4
            0x00, 0x00, 0x01, 0x00, 0x00, 0x00,
        ];
        let status = dispatch_changer_cdb(&mut state, &ie_status_cdb, &[], profile.clone());
        assert_eq!(status.status, SCSI_STATUS_GOOD);
        assert_eq!(state.changer_ie_count(), 4);
        assert_eq!(u16::from_be_bytes([status.reply[2], status.reply[3]]), 4);

        let mode_sense = dispatch_changer_cdb(
            &mut state,
            &[0x5A, 0x00, 0x1D, 0x00, 0x00, 0x00, 0x00, 0x00, 0x40, 0x00],
            &[],
            profile,
        );
        assert_eq!(mode_sense.status, SCSI_STATUS_GOOD);
        let assignment_page = &mode_sense.reply[8..28];
        assert_eq!(
            u16::from_be_bytes([assignment_page[10], assignment_page[11]]),
            0x0300
        );
        assert_eq!(
            u16::from_be_bytes([assignment_page[12], assignment_page[13]]),
            4
        );

        let _ = std::fs::remove_file(shared_changer_ie_path(&key));
    }

    #[test]
    fn test_changer_export_to_ie_auto_archives_into_shared_vault() {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let key = format!("lib-auto-vault-{nanos}__drivea");
        let mut state = crate::scsi_tape::state::TapeState::new(key.clone());
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");
        let _ = std::fs::remove_file(shared_changer_ie_path(&key));
        let _ = std::fs::remove_file(shared_changer_vault_path(&key));

        let export_cdb = vec![
            0xA5, 0x00, // MOVE MEDIUM
            0x00, 0x00, // transport address
            0x04, 0x00, // source slot 0x0400
            0x03, 0x00, // destination IE 0x0300
            0x00, 0x00, 0x00, 0x00,
        ];
        let response = dispatch_changer_cdb(&mut state, &export_cdb, &[], profile);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert_eq!(
            state
                .changer_ie_ports
                .get(&0x0300)
                .and_then(|value| value.as_deref()),
            None
        );
        let persisted_ie = read_shared_changer_ie(&key)
            .expect("read shared IE")
            .expect("shared IE payload");
        assert_eq!(persisted_ie.len(), 1);
        assert_eq!(persisted_ie[0], None);
        let persisted_vault = read_shared_changer_vault(&key)
            .expect("read shared vault")
            .expect("shared vault payload");
        assert_eq!(persisted_vault, vec![Some("VTL000001".to_string())]);

        let _ = std::fs::remove_file(shared_changer_ie_path(&key));
        let _ = std::fs::remove_file(shared_changer_vault_path(&key));
    }

    #[test]
    fn test_changer_auto_archive_ie_persist_failure_returns_check_condition() {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let key = format!("lib-auto-ie-fail-{nanos}__drivea");
        let mut state = crate::scsi_tape::state::TapeState::new(key.clone());
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");
        let ie_path = shared_changer_ie_path(&key);
        let vault_path = shared_changer_vault_path(&key);
        let _ = std::fs::remove_file(&ie_path);
        let _ = std::fs::remove_file(&vault_path);
        let _ = std::fs::remove_dir_all(&ie_path);
        std::fs::create_dir_all(&ie_path).expect("create unwritable IE target");

        let export_cdb = vec![
            0xA5, 0x00, 0x00, 0x00, 0x04, 0x00, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00,
        ];
        let response = dispatch_changer_cdb(&mut state, &export_cdb, &[], profile);

        assert_ne!(response.status, SCSI_STATUS_GOOD);
        assert_eq!(response.sense.get(2).copied().unwrap_or(0) & 0x0F, 0x04);
        assert_eq!(response.sense.get(12).copied().unwrap_or(0), 0x44);

        let _ = std::fs::remove_dir_all(&ie_path);
        let _ = std::fs::remove_file(&vault_path);
    }

    #[test]
    fn test_changer_auto_archive_vault_persist_failure_returns_check_condition() {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let key = format!("lib-auto-vault-fail-{nanos}__drivea");
        let mut state = crate::scsi_tape::state::TapeState::new(key.clone());
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");
        let ie_path = shared_changer_ie_path(&key);
        let vault_path = shared_changer_vault_path(&key);
        let _ = std::fs::remove_file(&ie_path);
        let _ = std::fs::remove_file(&vault_path);
        let _ = std::fs::remove_dir_all(&vault_path);
        std::fs::create_dir_all(&vault_path).expect("create unwritable vault target");

        let export_cdb = vec![
            0xA5, 0x00, 0x00, 0x00, 0x04, 0x00, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00,
        ];
        let response = dispatch_changer_cdb(&mut state, &export_cdb, &[], profile);

        assert_ne!(response.status, SCSI_STATUS_GOOD);
        assert_eq!(response.sense.get(2).copied().unwrap_or(0) & 0x0F, 0x04);
        assert_eq!(response.sense.get(12).copied().unwrap_or(0), 0x44);

        let _ = std::fs::remove_file(&ie_path);
        let _ = std::fs::remove_dir_all(&vault_path);
    }

    #[test]
    fn test_changer_initialize_element_status_with_range_resets_inventory() {
        let mut state = crate::scsi_tape::state::TapeState::new("changer-reset");
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");

        let move_cdb = vec![
            0xA5, 0x00, 0x00, 0x00, 0x04, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00,
        ];
        let move_response = dispatch_changer_cdb(&mut state, &move_cdb, &[], profile.clone());
        assert_eq!(move_response.status, SCSI_STATUS_GOOD);

        let init_range_cdb = vec![0x37, 0x00, 0x04, 0x00, 0x00, 0x10, 0x00, 0x00, 0x00, 0x00];
        let init_response = dispatch_changer_cdb(&mut state, &init_range_cdb, &[], profile.clone());
        assert_eq!(init_response.status, SCSI_STATUS_GOOD);

        let drive_status_cdb = vec![
            0xB8, 0x04, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x80, 0x00, 0x00,
        ];
        let drive_status = dispatch_changer_cdb(&mut state, &drive_status_cdb, &[], profile);
        assert_eq!(drive_status.status, SCSI_STATUS_GOOD);
        let drive_descriptor_offset = 16usize;
        assert_eq!(drive_status.reply[drive_descriptor_offset + 2] & 0x01, 0x00);
    }

    #[test]
    fn test_changer_initialize_element_status_applies_shared_slot_inventory() {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let serial = format!("changer-shared-slots-{nanos}");
        let mut state = crate::scsi_tape::state::TapeState::new(serial.clone());
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");
        write_shared_changer_slots(
            &serial,
            &[
                Some("ADD000001".to_string()),
                None,
                Some("ADD000003".to_string()),
            ],
        )
        .expect("write shared slots");

        let init_cdb = vec![0x07, 0x00, 0x00, 0x00, 0x00, 0x00];
        let response = dispatch_changer_cdb(&mut state, &init_cdb, &[], profile);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert_eq!(
            state.changer_slots.get(&0x0400).and_then(|v| v.as_deref()),
            Some("ADD000001")
        );
        assert_eq!(
            state.changer_slots.get(&0x0401).and_then(|v| v.as_deref()),
            None
        );
        assert_eq!(
            state.changer_slots.get(&0x0402).and_then(|v| v.as_deref()),
            Some("ADD000003")
        );

        let slots_path = shared_changer_slots_path(&serial);
        let _ = std::fs::remove_file(slots_path);
    }

    #[test]
    fn test_changer_reports_dynamic_slot_count_from_shared_inventory() {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let serial = format!("changer-dynamic-slot-count-{nanos}");
        let mut state = crate::scsi_tape::state::TapeState::new(serial.clone());
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");
        let slots = (0..20usize)
            .map(|idx| Some(format!("VTA{idx:03}L06")))
            .collect::<Vec<_>>();
        write_shared_changer_slots(&serial, &slots).expect("write shared slots");

        let mode_sense_cdb = vec![0x1A, 0x00, 0x1D, 0x00, 0x40, 0x00];
        let mode_sense = dispatch_changer_cdb(&mut state, &mode_sense_cdb, &[], profile.clone());
        assert_eq!(mode_sense.status, SCSI_STATUS_GOOD);
        let assignment_page_offset = 4usize;
        assert!(mode_sense.reply.len() >= assignment_page_offset + 10);
        let storage_slot_count = u16::from_be_bytes([
            mode_sense.reply[assignment_page_offset + 8],
            mode_sense.reply[assignment_page_offset + 9],
        ]);
        assert_eq!(storage_slot_count, 20);

        let read_status_cdb = vec![
            0xB8, 0x02, 0x04, 0x00, 0x00, 0x20, 0x00, 0x00, 0x08, 0x00, 0x00, 0x00,
        ];
        let read_status = dispatch_changer_cdb(&mut state, &read_status_cdb, &[], profile);
        assert_eq!(read_status.status, SCSI_STATUS_GOOD);
        assert!(read_status.reply.len() >= 4);
        let reported = u16::from_be_bytes([read_status.reply[2], read_status.reply[3]]);
        assert_eq!(reported, 20);
        assert_eq!(
            state
                .changer_slots
                .get(&0x0413)
                .and_then(|value| value.as_deref()),
            Some("VTA019L06")
        );

        let slots_path = shared_changer_slots_path(&serial);
        let _ = std::fs::remove_file(slots_path);
    }

    #[test]
    fn test_changer_report_luns_returns_lun0() {
        let mut state = crate::scsi_tape::state::TapeState::new("changer-report-luns");
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");
        let cdb = vec![
            0xA0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x20, 0x00, 0x00, 0x00,
        ];
        let response = dispatch_changer_cdb(&mut state, &cdb, &[], profile);

        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert!(response.reply.len() >= 16);
        assert_eq!(&response.reply[0..4], &[0x00, 0x00, 0x00, 0x08]);
    }

    #[test]
    fn test_changer_unsupported_opcode_returns_deterministic_sense() {
        let mut state = crate::scsi_tape::state::TapeState::new("changer-unsup");
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");
        let response_34 = dispatch_changer_cdb(
            &mut state,
            &[0x34, 0, 0, 0, 0, 0, 0, 0, 0, 0],
            &[],
            profile.clone(),
        );
        let response_ff =
            dispatch_changer_cdb(&mut state, &[0xFF, 0, 0, 0, 0, 0, 0, 0, 0, 0], &[], profile);

        for resp in [response_34, response_ff] {
            assert_eq!(resp.status, SCSI_STATUS_CHECK_CONDITION);
            assert!(resp.sense.len() >= 14);
            assert_eq!(resp.sense[2] & 0x0F, 0x05); // ILLEGAL REQUEST
            assert_eq!(resp.sense[12], 0x20); // INVALID COMMAND OPERATION CODE
            assert_eq!(resp.sense[13], 0x00);
        }
    }

    #[test]
    fn test_drive_read_block_limits_reports_non_zero_range() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-rbl");
        let cdb = vec![0x05, 0x00, 0x00, 0x00, 0x00, 0x00];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert_eq!(response.reply.len(), 6);

        let max_block = ((response.reply[1] as u32) << 16)
            | ((response.reply[2] as u32) << 8)
            | response.reply[3] as u32;
        let min_block = u16::from_be_bytes([response.reply[4], response.reply[5]]) as u32;
        assert!(max_block >= 1024);
        assert!(min_block >= 1);
    }

    #[test]
    fn test_drive_read_block_limits_mloi_matches_legacy_value() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-rbl-mloi");
        let cdb = vec![0x05, 0x01, 0x00, 0x00, 0x00, 0x00];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert_eq!(response.reply.len(), 20);
        let mloi = u64::from_be_bytes([
            response.reply[12],
            response.reply[13],
            response.reply[14],
            response.reply[15],
            response.reply[16],
            response.reply[17],
            response.reply[18],
            response.reply[19],
        ]);
        assert_eq!(mloi, 0x0000_0000_FFFF_FFFFu64);
    }

    #[test]
    fn test_drive_mode_sense_10_exposes_descriptor_and_pages_without_media() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-ms10");
        let cdb = vec![0x5A, 0x00, 0x3F, 0x00, 0x00, 0x00, 0x00, 0x00, 0x80, 0x00];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert!(response.reply.len() >= 24);
        assert_eq!(&response.reply[6..8], &[0x00, 0x08]); // block descriptor length
        assert_ne!(response.reply[8], 0x00); // density code should not be zero
        assert!(
            response.reply.windows(2).any(|pair| pair == [0x0F, 0x0E]),
            "expected compression mode page in MODE SENSE(10) response"
        );
        assert!(
            response.reply.windows(2).any(|pair| pair == [0x02, 0x0E]),
            "expected disconnect/reconnect mode page with legacy length"
        );
    }

    #[test]
    fn test_drive_mode_sense_6_vendor_page_probe_is_accepted() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-ms6-page00");
        let cdb = vec![0x1A, 0x00, 0x00, 0x00, 0x20, 0x00];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert!(response.reply.len() >= 4);
    }

    #[test]
    fn test_drive_mode_sense_10_sets_wp_for_loaded_worm_media() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-ms10-loaded");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        state.retention_policy.is_worm_media = true;
        state.retention_policy.retention_locked = true;

        let cdb = vec![0x5A, 0x00, 0x10, 0x00, 0x00, 0x00, 0x00, 0x00, 0x40, 0x00];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert!(response.reply.len() >= 8);
        assert_eq!(response.reply[2], 0x00);
        assert_eq!(
            response.reply[3] & 0x80,
            0x80,
            "WP bit should be set for locked WORM"
        );
    }

    #[test]
    fn test_drive_mode_sense_saved_values_returns_saving_parameters_not_supported() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-ms-saved");
        let cdb = vec![0x5A, 0x00, 0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x40, 0x00];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(response.status, SCSI_STATUS_CHECK_CONDITION);
        assert!(response.sense.len() >= 14);
        assert_eq!(response.sense[2] & 0x0F, 0x05);
        assert_eq!(response.sense[12], 0x39);
        assert_eq!(response.sense[13], 0x00);
    }

    #[test]
    fn test_erase_cdb_decodes_short_and_long_modes() {
        let state = crate::scsi_tape::state::TapeState::new("drive-erase-decode");

        match decode_cdb_to_command(&state, 0x19, &[0x19, 0x00, 0, 0, 0, 0], &[]) {
            Some(crate::scsi_tape::commands_core::CoreCommand::Erase { mode }) => {
                assert_eq!(mode, crate::scsi_tape::command_chain::EraseMode::Short);
            }
            other => panic!("unexpected short erase decode: {other:?}"),
        }

        match decode_cdb_to_command(&state, 0x19, &[0x19, 0x01, 0, 0, 0, 0], &[]) {
            Some(crate::scsi_tape::commands_core::CoreCommand::Erase { mode }) => {
                assert_eq!(mode, crate::scsi_tape::command_chain::EraseMode::Long);
            }
            other => panic!("unexpected long erase decode: {other:?}"),
        }
    }

    #[test]
    fn test_drive_mode_sense_10_medium_partition_page_uses_legacy_length() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-ms10-page11");
        let cdb = vec![0x5A, 0x00, 0x11, 0x00, 0x00, 0x00, 0x00, 0x00, 0x80, 0x00];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert!(response.reply.windows(2).any(|pair| pair == [0x11, 0x0E]));
    }

    #[test]
    fn test_drive_mode_sense_10_medium_partition_page_reports_max_additional_partitions() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-ms10-page11-max");
        let cdb = vec![0x5A, 0x00, 0x11, 0x00, 0x00, 0x00, 0x00, 0x00, 0x80, 0x00];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        let idx = response
            .reply
            .windows(2)
            .position(|pair| pair == [0x11, 0x0E])
            .expect("missing medium partition page");
        assert_eq!(response.reply[idx + 2], 0x03); // max_addl_partitions for LTO-6
        assert_eq!(response.reply[idx + 3], 0x00); // addl_partitions_defined default
    }

    #[test]
    fn test_drive_mode_sense_changeable_pages_keep_mutable_bits() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-ms10-changeable-bits");

        let compression = dispatch_raw_cdb(
            &mut state,
            &[0x5A, 0x00, 0x4F, 0x00, 0x00, 0x00, 0x00, 0x00, 0x80, 0x00],
            &[],
        );
        assert_eq!(compression.status, SCSI_STATUS_GOOD);
        let compression_idx = compression
            .reply
            .windows(2)
            .position(|pair| pair == [0x0F, 0x0E])
            .expect("missing data compression page");
        assert_eq!(compression.reply[compression_idx + 2], 0x80); // DCE bit writable

        let device_cfg = dispatch_raw_cdb(
            &mut state,
            &[0x5A, 0x00, 0x50, 0x00, 0x00, 0x00, 0x00, 0x00, 0x80, 0x00],
            &[],
        );
        assert_eq!(device_cfg.status, SCSI_STATUS_GOOD);
        let device_idx = device_cfg
            .reply
            .windows(2)
            .position(|pair| pair == [0x10, 0x0E])
            .expect("missing device configuration page");
        assert_eq!(device_cfg.reply[device_idx + 10], 0x08); // SEW writable
        assert_eq!(device_cfg.reply[device_idx + 14], 0xFF); // compression algorithm writable
    }

    #[test]
    fn test_drive_mode_sense_hides_compression_capability_when_policy_disables_it() {
        let mut state =
            crate::scsi_tape::state::TapeState::new("drive-ms10-compression-policy-off");
        state.data_compression_allowed = false;
        state.data_compression_enabled = false;

        let current = dispatch_raw_cdb(
            &mut state,
            &[0x5A, 0x00, 0x0F, 0x00, 0x00, 0x00, 0x00, 0x00, 0x40, 0x00],
            &[],
        );
        assert_eq!(current.status, SCSI_STATUS_GOOD);
        let current_idx = current
            .reply
            .windows(2)
            .position(|pair| pair == [0x0F, 0x0E])
            .expect("missing data compression page");
        assert_eq!(current.reply[current_idx + 2], 0x00);

        let changeable = dispatch_raw_cdb(
            &mut state,
            &[0x5A, 0x00, 0x4F, 0x00, 0x00, 0x00, 0x00, 0x00, 0x40, 0x00],
            &[],
        );
        assert_eq!(changeable.status, SCSI_STATUS_GOOD);
        let changeable_idx = changeable
            .reply
            .windows(2)
            .position(|pair| pair == [0x0F, 0x0E])
            .expect("missing data compression page");
        assert_eq!(changeable.reply[changeable_idx + 2], 0x00);
    }

    #[test]
    fn test_drive_sync_mounts_and_unmounts_from_shared_state() {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let serial = format!("drive-shared-{nanos}");
        let state_path = shared_media_state_path(&serial);
        let _ = std::fs::remove_file(&state_path);

        let mut state = crate::scsi_tape::state::TapeState::new(serial.clone());
        assert_eq!(
            state.mount_state,
            crate::scsi_tape::state::MountState::Empty
        );
        assert!(state.cartridge_id.is_none());

        write_shared_loaded_cartridge(&serial, Some("VTL000777")).expect("write shared");
        sync_drive_mount_from_shared(&mut state);
        assert_eq!(
            state.mount_state,
            crate::scsi_tape::state::MountState::Loaded
        );
        assert_eq!(state.cartridge_id.as_deref(), Some("VTL000777"));
        assert_eq!(state.current_position, 0);
        assert_eq!(state.eod_position, 0);

        write_shared_loaded_cartridge(&serial, None).expect("write shared");
        sync_drive_mount_from_shared(&mut state);
        assert_eq!(
            state.mount_state,
            crate::scsi_tape::state::MountState::Empty
        );
        assert!(state.cartridge_id.is_none());
    }

    #[test]
    fn test_drive_sync_bypasses_stale_loaded_state_cache() {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let serial = format!("drive-stale-cache-{nanos}");
        let state_path = shared_media_state_path(&serial);
        let _ = std::fs::remove_file(&state_path);

        write_shared_loaded_cartridge(&serial, Some("VTL000001")).expect("write shared");
        let mut state = crate::scsi_tape::state::TapeState::new(serial.clone());
        sync_drive_mount_from_shared(&mut state);
        assert_eq!(state.cartridge_id.as_deref(), Some("VTL000001"));

        std::fs::write(&state_path, "cartridge=VTL000002\n").expect("external state update");
        sync_drive_mount_from_shared(&mut state);
        assert_eq!(state.cartridge_id.as_deref(), Some("VTL000002"));

        let _ = write_shared_loaded_cartridge(&serial, None);
        let _ = std::fs::remove_file(&state_path);
    }

    #[test]
    fn test_read_position_short_long_extended_encoding() {
        let mut state = crate::scsi_tape::state::TapeState::new("read-pos-encoding");
        state.block_mode.mode = crate::scsi_tape::state::BlockMode::Fixed;
        state.block_mode.fixed_block_size = 1;
        let report = crate::scsi_tape::command_chain::PositionReport {
            current_position: 0x1122_3344,
            eod_position: 0x5566_7788,
            file_number: 3,
            set_number: 1,
            filemark_count: 7,
            next_filemark: None,
            early_warning: false,
        };

        let short =
            read_position_response_bytes(&state, &[0x34, 0x00, 0, 0, 0, 0, 0, 0, 0, 0], &report, 0);
        assert_eq!(short.len(), 20);
        assert_eq!(short[1], 0x00);
        assert_eq!(&short[4..8], &[0x11, 0x22, 0x33, 0x44]);
        assert_eq!(&short[8..12], &[0x11, 0x22, 0x33, 0x44]);

        let long =
            read_position_response_bytes(&state, &[0x34, 0x06, 0, 0, 0, 0, 0, 0, 0, 0], &report, 2);
        assert_eq!(long.len(), 32);
        assert_eq!(&long[4..8], &2u32.to_be_bytes());
        assert_eq!(&long[8..16], &0x1122_3344u64.to_be_bytes());
        assert_eq!(&long[16..24], &3u64.to_be_bytes());
        assert_eq!(&long[24..32], &1u64.to_be_bytes());

        let extended = read_position_response_bytes(
            &state,
            &[0x34, 0x08, 0, 0, 0, 0, 0, 0x00, 0x20, 0x00],
            &report,
            0,
        );
        assert_eq!(extended.len(), 32);
        assert_eq!(extended[1], 0x00);
        assert_eq!(&extended[2..4], &0x001Cu16.to_be_bytes());
        assert_eq!(&extended[8..16], &0x1122_3344u64.to_be_bytes());
        assert_eq!(&extended[16..24], &0x1122_3344u64.to_be_bytes());
        assert_eq!(&extended[24..32], &0u64.to_be_bytes());
    }

    #[test]
    fn test_read_position_reports_bot_as_block_zero_with_bop() {
        let state = crate::scsi_tape::state::TapeState::new("read-pos-bot");
        let report = crate::scsi_tape::command_chain::PositionReport {
            current_position: 0,
            eod_position: 0,
            file_number: 0,
            set_number: 0,
            filemark_count: 0,
            next_filemark: None,
            early_warning: false,
        };

        let short =
            read_position_response_bytes(&state, &[0x34, 0x00, 0, 0, 0, 0, 0, 0, 0, 0], &report, 0);
        assert_eq!(short[0] & 0x80, 0x80, "BOP should be set at BOT");
        assert_eq!(&short[4..8], &0u32.to_be_bytes());
        assert_eq!(&short[8..12], &0u32.to_be_bytes());

        let long =
            read_position_response_bytes(&state, &[0x34, 0x06, 0, 0, 0, 0, 0, 0, 0, 0], &report, 0);
        assert_eq!(long[0] & 0x80, 0x80, "BOP should be set at BOT");
        assert_eq!(&long[8..16], &0u64.to_be_bytes());

        let extended = read_position_response_bytes(
            &state,
            &[0x34, 0x08, 0, 0, 0, 0, 0, 0, 0x20, 0],
            &report,
            0,
        );
        assert_eq!(extended[0] & 0x80, 0x80, "BOP should be set at BOT");
        assert_eq!(&extended[8..16], &0u64.to_be_bytes());
        assert_eq!(&extended[16..24], &0u64.to_be_bytes());
    }

    #[test]
    fn test_read_position_reports_fixed_block_indices_for_host() {
        let mut state = crate::scsi_tape::state::TapeState::new("read-pos-fixed");
        state.block_mode.mode = crate::scsi_tape::state::BlockMode::Fixed;
        state.block_mode.fixed_block_size = 262_144;
        let report = crate::scsi_tape::command_chain::PositionReport {
            current_position: 262_144,
            eod_position: 524_288,
            file_number: 0,
            set_number: 0,
            filemark_count: 0,
            next_filemark: None,
            early_warning: false,
        };

        let short =
            read_position_response_bytes(&state, &[0x34, 0x00, 0, 0, 0, 0, 0, 0, 0, 0], &report, 0);
        assert_eq!(&short[4..8], &1u32.to_be_bytes());
    }

    #[test]
    fn test_read_position_reports_variable_block_indices_for_host() {
        let mut state = crate::scsi_tape::state::TapeState::new("read-pos-variable");
        state.block_mode.mode = crate::scsi_tape::state::BlockMode::Variable;
        state.block_mode.fixed_block_size = 0;
        state.block_starts = vec![0, 6, 7];
        state.filemarks = vec![6];
        state.current_position = 11;
        state.eod_position = 11;
        let report = crate::scsi_tape::command_chain::PositionReport {
            current_position: 11,
            eod_position: 11,
            file_number: 1,
            set_number: 0,
            filemark_count: 1,
            next_filemark: None,
            early_warning: false,
        };

        let short =
            read_position_response_bytes(&state, &[0x34, 0x00, 0, 0, 0, 0, 0, 0, 0, 0], &report, 0);
        assert_eq!(&short[4..8], &3u32.to_be_bytes());
    }

    #[test]
    fn test_decode_locate_uses_fixed_block_to_internal_offset() {
        let mut state = crate::scsi_tape::state::TapeState::new("decode-locate-fixed");
        state.block_mode.mode = crate::scsi_tape::state::BlockMode::Fixed;
        state.block_mode.fixed_block_size = 262_144;
        let cdb = [0x2B, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00];
        let cmd = decode_cdb_to_command(&state, 0x2B, &cdb, &[]).expect("locate decode");
        match cmd {
            CoreCommand::Locate { logical_block } => assert_eq!(logical_block, 262_144),
            _ => panic!("expected locate command"),
        }
    }

    #[test]
    fn test_decode_locate_uses_variable_block_indices() {
        let mut state = crate::scsi_tape::state::TapeState::new("decode-locate-variable");
        state.block_mode.mode = crate::scsi_tape::state::BlockMode::Variable;
        state.block_mode.fixed_block_size = 0;
        state.block_starts = vec![0, 6, 7];
        state.filemarks = vec![6];
        state.eod_position = 11;
        let cdb = [0x2B, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00];
        let cmd = decode_cdb_to_command(&state, 0x2B, &cdb, &[]).expect("locate decode");
        match cmd {
            CoreCommand::Locate { logical_block } => assert_eq!(logical_block, 7),
            _ => panic!("expected locate command"),
        }
    }

    #[test]
    fn test_locate16_translates_fixed_block_index_to_offset() {
        let mut state = crate::scsi_tape::state::TapeState::new("locate16-fixed");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        state.block_mode.mode = crate::scsi_tape::state::BlockMode::Fixed;
        state.block_mode.fixed_block_size = 262_144;
        state.eod_position = 10 * 262_144;
        let cdb = [
            0x92, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00,
            0x00, 0x00,
        ];
        let response = locate_16_drive(&mut state, &cdb);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert_eq!(state.current_position, 262_144);
    }

    #[test]
    fn test_locate16_translates_variable_block_index_to_offset() {
        let mut state = crate::scsi_tape::state::TapeState::new("locate16-variable");
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let cartridge_id = format!("VTAREADPOS{nanos}");
        crate::media::mount_bridge::attach_cartridge(&mut state, &cartridge_id).expect("attach");
        write_shared_loaded_cartridge(&state.drive_id, Some(&cartridge_id))
            .expect("write shared state");
        drain_unit_attention(&mut state);
        state.block_mode.mode = crate::scsi_tape::state::BlockMode::Variable;
        state.block_mode.fixed_block_size = 0;

        let write_a = vec![0x0A, 0x00, 0x00, 0x00, 0x00, 0x00];
        let write_a_payload = b"blockA".to_vec();
        let write_a_response = dispatch_raw_cdb(&mut state, &write_a, &write_a_payload);
        assert_eq!(write_a_response.status, SCSI_STATUS_GOOD);

        let write_b = vec![0x0A, 0x00, 0x00, 0x00, 0x00, 0x00];
        let write_b_payload = b"BLOB".to_vec();
        let write_b_response = dispatch_raw_cdb(&mut state, &write_b, &write_b_payload);
        assert_eq!(write_b_response.status, SCSI_STATUS_GOOD);

        let cdb = [
            0x92, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00,
            0x00, 0x00,
        ];
        let response = locate_16_drive(&mut state, &cdb);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert_eq!(state.current_position, 6);

        let read_cdb = vec![0x08, 0x00, 0x00, 0x00, 0x00, 0x00];
        let read_response = dispatch_raw_cdb(&mut state, &read_cdb, &[]);
        assert_eq!(read_response.status, SCSI_STATUS_GOOD);
        assert_eq!(read_response.reply, b"BLOB");
    }

    #[test]
    fn test_drive_read_position_rejects_unknown_service_action() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-read-pos-invalid-sa");
        let response = dispatch_raw_cdb(&mut state, &[0x34, 0x01, 0, 0, 0, 0, 0, 0, 0, 0], &[]);
        assert_eq!(response.status, SCSI_STATUS_CHECK_CONDITION);
        assert!(response.sense.len() >= 14);
        assert_eq!(response.sense[2] & 0x0F, 0x05);
        assert_eq!(response.sense[12], 0x24);
        assert_eq!(response.sense[13], 0x00);
    }

    #[test]
    fn test_drive_report_density_support_returns_descriptor_with_capacity() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-density");
        let cdb = vec![0x44, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert!(response.reply.len() >= 56);

        let avail_len = u16::from_be_bytes([response.reply[0], response.reply[1]]);
        assert!(avail_len >= 54);
        assert_ne!(response.reply[4], 0x00); // primary density code
        let capacity_mib = u32::from_be_bytes([
            response.reply[16],
            response.reply[17],
            response.reply[18],
            response.reply[19],
        ]);
        assert!(capacity_mib > 0);
    }

    #[test]
    fn test_drive_report_density_support_medium_type_descriptor() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-density-medium");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        state.retention_policy.is_worm_media = false;
        let cdb = vec![0x44, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert!(response.reply.len() >= 56);
        assert_eq!(&response.reply[6..8], &0x0034u16.to_be_bytes());
        assert_eq!(&response.reply[18..20], &127u16.to_be_bytes());
        assert_eq!(&response.reply[32..36], b"Data");
    }

    #[test]
    fn test_drive_read_attribute_reports_capacity_attributes() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-attr");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        state.cartridge_id = Some("VTL000321".to_string());
        write_shared_loaded_cartridge("drive-attr", Some("VTL000321")).expect("write shared state");
        drain_unit_attention(&mut state);
        let cdb = vec![
            0x8C, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // op/service + partition
            0x00, 0x00, // first attribute
            0x00, 0x00, 0x10, 0x00, // allocation length
            0x00, 0x00,
        ];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert!(response.reply.len() >= 4);
        let payload_len = u32::from_be_bytes([
            response.reply[0],
            response.reply[1],
            response.reply[2],
            response.reply[3],
        ]) as usize;
        assert!(payload_len > 0);
        assert!(
            response.reply.windows(2).any(|pair| pair == [0x00, 0x01]),
            "expected maximum capacity attribute id"
        );
        assert!(
            response.reply.windows(2).any(|pair| pair == [0x04, 0x08]),
            "expected medium type attribute id"
        );
        let _ = write_shared_loaded_cartridge("drive-attr", None);
    }

    #[test]
    fn test_drive_read_attribute_capacity_is_reported_in_mib_units() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-attr-cap-mib");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        let profile = crate::scsi_tape::profiles::resolve_drive_profile("ibm-ult3580-td6");
        let expected_mib = bytes_to_mib_u32(native_capacity_bytes_for_profile(&profile)) as u64;

        let cdb = vec![
            0x8C, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // op/service + partition
            0x00, 0x01, // first attribute (max capacity)
            0x00, 0x00, 0x00, 0x40, // allocation length
            0x00, 0x00,
        ];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert!(response.reply.len() >= 17);
        assert_eq!(&response.reply[4..6], &[0x00, 0x01]);
        assert_eq!(response.reply[6], 0x80);
        assert_eq!(&response.reply[7..9], &[0x00, 0x08]);
        let capacity = u64::from_be_bytes([
            response.reply[9],
            response.reply[10],
            response.reply[11],
            response.reply[12],
            response.reply[13],
            response.reply[14],
            response.reply[15],
            response.reply[16],
        ]);
        assert_eq!(capacity, expected_mib);
    }

    #[test]
    fn test_drive_read_attribute_0401_reports_loaded_medium_serial() {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let drive_id = format!("IBMvtldrv010-{nanos}");
        let cartridge_id = format!("DBK{nanos}");
        let mut state = crate::scsi_tape::state::TapeState::new(drive_id.clone());
        crate::media::mount_bridge::attach_cartridge(&mut state, &cartridge_id).expect("attach");
        write_shared_loaded_cartridge(&drive_id, Some(&cartridge_id)).expect("write shared state");
        drain_unit_attention(&mut state);

        let cdb = vec![
            0x8C, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // op/service + partition
            0x04, 0x01, // first attribute: medium serial number
            0x00, 0x00, 0x04, 0x00, // Dbackup-sized allocation length
            0x00, 0x00,
        ];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(
            response.status, SCSI_STATUS_GOOD,
            "sense={:?}",
            response.sense
        );
        assert!(response.reply.len() >= 41);
        assert_eq!(&response.reply[4..6], &[0x04, 0x01]);
        assert_eq!(response.reply[6], 0x81);
        assert_eq!(&response.reply[7..9], &[0x00, 0x20]);
        assert_eq!(
            &response.reply[9..9 + cartridge_id.len()],
            cartridge_id.as_bytes()
        );
        let serial =
            std::str::from_utf8(&response.reply[9..41]).expect("medium serial should be ASCII");
        assert!(
            !serial.contains(&drive_id),
            "medium serial must not be derived from drive id: {serial:?}"
        );

        let _ = write_shared_loaded_cartridge(&drive_id, None);
    }

    #[test]
    fn test_drive_read_attribute_avoids_partial_entries_when_allocation_truncates() {
        let drive_id = "drive-attr-truncate";
        let cartridge_id = "VTA000L09";
        let mut state = crate::scsi_tape::state::TapeState::new(drive_id);
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        state.cartridge_id = Some(cartridge_id.to_string());
        write_shared_loaded_cartridge(drive_id, Some(cartridge_id)).expect("write shared state");
        drain_unit_attention(&mut state);

        let cdb = vec![
            0x8C, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // op/service + partition
            0x00, 0x00, // first attribute
            0x00, 0x00, 0x04, 0x00, // Dbackup-sized allocation length
            0x00, 0x00,
        ];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(
            response.status, SCSI_STATUS_GOOD,
            "sense={:?}",
            response.sense
        );
        assert!(response.reply.len() <= 1024);
        assert!(response.reply.len() >= 4);
        let payload_len = u32::from_be_bytes([
            response.reply[0],
            response.reply[1],
            response.reply[2],
            response.reply[3],
        ]) as usize;
        assert_eq!(payload_len, response.reply.len().saturating_sub(4));

        let mut offset = 4usize;
        let mut seen = Vec::new();
        while offset < response.reply.len() {
            assert!(
                response.reply.len().saturating_sub(offset) >= 5,
                "truncated attribute header at offset {offset}"
            );
            let attr = u16::from_be_bytes([response.reply[offset], response.reply[offset + 1]]);
            let value_len =
                u16::from_be_bytes([response.reply[offset + 3], response.reply[offset + 4]])
                    as usize;
            let next = offset + 5 + value_len;
            assert!(
                next <= response.reply.len(),
                "truncated attribute 0x{attr:04X}"
            );
            assert!(!seen.contains(&attr), "duplicated attribute 0x{attr:04X}");
            seen.push(attr);
            offset = next;
        }
        let _ = write_shared_loaded_cartridge(drive_id, None);
    }

    #[test]
    fn test_drive_shared_cartridge_metadata_overrides_reported_capacity() {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let cartridge_id = format!("CAP{nanos}");
        let _ = fs::remove_file(shared_cartridge_metadata_path(&cartridge_id));
        let capacity_bytes = 42 * 1024 * 1024 * 1024u64;
        write_shared_cartridge_metadata(&cartridge_id, Some(capacity_bytes), 0)
            .expect("write cartridge metadata");

        let mut state = crate::scsi_tape::state::TapeState::new(format!("drive-cap-{nanos}"));
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        state.cartridge_id = Some(cartridge_id.clone());
        write_shared_loaded_cartridge(&state.drive_id, Some(&cartridge_id))
            .expect("write loaded cartridge state");
        apply_shared_cartridge_metadata(&mut state);

        assert_eq!(attr_u64(&mut state, 0x0001), 42 * 1024);
        assert_eq!(attr_u64(&mut state, 0x0407), 4096);

        let mode_sense =
            dispatch_raw_cdb(&mut state, &[0x5A, 0x00, 0x10, 0, 0, 0, 0, 0, 0x40, 0], &[]);
        assert_eq!(mode_sense.status, SCSI_STATUS_GOOD);
        assert_eq!(mode_sense.reply[2], 0x00);
        assert_eq!(&mode_sense.reply[6..8], &[0x00, 0x08]);
        let reported_blocks = u32::from_be_bytes([
            0x00,
            mode_sense.reply[9],
            mode_sense.reply[10],
            mode_sense.reply[11],
        ]);
        assert_eq!(
            reported_blocks,
            (capacity_bytes / u64::from(DRIVE_DEFAULT_BLOCK_LENGTH)) as u32
        );
        let reported_block_len = u32::from_be_bytes([
            0x00,
            mode_sense.reply[13],
            mode_sense.reply[14],
            mode_sense.reply[15],
        ]);
        assert_eq!(reported_block_len, DRIVE_DEFAULT_BLOCK_LENGTH);

        let medium_descriptor =
            dispatch_raw_cdb(&mut state, &[0x44, 0x02, 0, 0, 0, 0, 0, 0x01, 0, 0], &[]);
        assert_eq!(medium_descriptor.status, SCSI_STATUS_GOOD);
        let expected_product = b"HOLO Custom Tape";
        assert!(
            medium_descriptor
                .reply
                .windows(expected_product.len())
                .any(|window| window == expected_product),
            "expected custom medium descriptor, got {:?}",
            medium_descriptor.reply
        );

        let log = dispatch_raw_cdb(
            &mut state,
            &[0x4D, 0x00, 0x31, 0, 0, 0, 0, 0x01, 0x00, 0],
            &[],
        );
        assert_eq!(log.status, SCSI_STATUS_GOOD, "sense={:?}", log.sense);
        assert_eq!(log_param_u32(&log.reply, 0x0003), Some(42 * 1024));

        let _ = fs::remove_file(shared_cartridge_metadata_path(&cartridge_id));
        let _ = write_shared_loaded_cartridge(&state.drive_id, None);
    }

    #[test]
    fn test_drive_write_syncs_used_bytes_to_shared_cartridge_metadata() {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let drive_id = format!("drive-used-{nanos}");
        let cartridge_id = format!("USED{nanos}");
        let _ = fs::remove_file(shared_cartridge_metadata_path(&cartridge_id));
        write_shared_cartridge_metadata(&cartridge_id, Some(32 * 1024 * 1024), 0)
            .expect("write cartridge metadata");

        let mut state = crate::scsi_tape::state::TapeState::new(drive_id);
        crate::media::mount_bridge::attach_cartridge(&mut state, &cartridge_id).expect("attach");
        write_shared_loaded_cartridge(&state.drive_id, Some(&cartridge_id))
            .expect("write shared state");
        apply_shared_cartridge_metadata(&mut state);
        drain_unit_attention(&mut state);
        state.block_mode.mode = crate::scsi_tape::state::BlockMode::Variable;
        state.block_mode.fixed_block_size = 0;

        let payload = vec![0xA5; 1024 * 1024];
        let write = dispatch_raw_cdb(&mut state, &[0x0A, 0, 0, 0, 1, 0], &payload);
        assert_eq!(write.status, SCSI_STATUS_GOOD, "sense={:?}", write.sense);

        let metadata = read_shared_cartridge_metadata(&cartridge_id)
            .expect("read cartridge metadata")
            .expect("cartridge metadata");
        assert_eq!(metadata.capacity_bytes, Some(32 * 1024 * 1024));
        assert_eq!(metadata.used_bytes, Some(payload.len() as u64));

        let _ = fs::remove_file(shared_cartridge_metadata_path(&cartridge_id));
    }

    #[test]
    fn test_drive_read_attribute_standard_mam_attributes_are_readonly() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-attr-0407");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;

        for (attr, expected_format) in [
            (0x0008u16, 0x81u8),
            (0x0009u16, 0x80u8),
            (0x020Au16, 0x81u8),
            (0x020Bu16, 0x81u8),
            (0x020Cu16, 0x81u8),
            (0x020Du16, 0x81u8),
            (0x0400u16, 0x81u8),
            (0x0404u16, 0x81u8),
            (0x0407u16, 0x80u8),
        ] {
            let (format, _) = read_attr_entry(&mut state, attr);
            assert_eq!(format, expected_format, "attribute 0x{attr:04X}");
        }

        assert_eq!(
            read_attr_value(&mut state, 0x0404),
            fixed_ascii_value("LTO-CVE", 8)
        );
        assert_eq!(attr_u64(&mut state, 0x0407), 4096);
    }

    #[test]
    fn test_drive_read_attribute_medium_type_is_binary_single_byte() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-attr-0408");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        let cdb = vec![
            0x8C, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // op/service + partition
            0x04, 0x08, // first attribute
            0x00, 0x00, 0x00, 0x40, // allocation length
            0x00, 0x00,
        ];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert!(response.reply.len() >= 10);
        assert_eq!(&response.reply[4..6], &[0x04, 0x08]);
        assert_eq!(response.reply[6], 0x80);
        assert_eq!(&response.reply[7..9], &[0x00, 0x01]);
        assert_eq!(response.reply[9], 0x00);
    }

    #[test]
    fn test_drive_read_attribute_list_includes_extended_ids() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-attr-list");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        let cdb = vec![
            0x8C, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // op/service + partition
            0x00, 0x00, // first attribute
            0x00, 0x00, 0x04, 0x00, // allocation length
            0x00, 0x00,
        ];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert!(response.reply.len() > 4);
        assert!(
            response.reply.windows(2).any(|pair| pair == [0x04, 0x07]),
            "expected 0x0407 MAM capacity in attribute list"
        );
        assert!(
            response.reply.windows(2).any(|pair| pair == [0x08, 0x0C]),
            "expected 0x080C coherency info in attribute list"
        );
        assert!(
            response.reply.windows(2).any(|pair| pair == [0x02, 0x20]),
            "expected 0x0220 life-write counter in attribute list"
        );
    }

    #[test]
    fn test_drive_write_attribute_updates_read_attribute_value() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-attr-write");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        state.cartridge_id = Some("VTLWATTR01".to_string());
        state.active_layout = Some(temp_layout_for_test("mam-write-readback"));
        write_shared_loaded_cartridge("drive-attr-write", Some("VTLWATTR01"))
            .expect("write shared state");

        let barcode = b"BARCODE-NEW";
        let mut payload = vec![0x08, 0x06, 0x01, 0x00, barcode.len() as u8];
        payload.extend_from_slice(barcode);
        let mut write_cdb = vec![0x8D, 0x00, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0];
        let payload_len = payload.len() as u32;
        write_cdb[10..14].copy_from_slice(&payload_len.to_be_bytes());
        let write_response = dispatch_raw_cdb(&mut state, &write_cdb, &payload);
        assert_eq!(
            write_response.status, SCSI_STATUS_GOOD,
            "sense={:?}",
            write_response.sense
        );
        drain_unit_attention(&mut state);

        let read_cdb = vec![
            0x8C, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // op/service + partition
            0x08, 0x06, // first attribute
            0x00, 0x00, 0x00, 0x80, // allocation length
            0x00, 0x00,
        ];
        let read_response = dispatch_raw_cdb(&mut state, &read_cdb, &[]);
        assert_eq!(read_response.status, SCSI_STATUS_GOOD);
        assert!(read_response.reply.len() >= 4 + 5 + 32);
        assert_eq!(&read_response.reply[4..6], &[0x08, 0x06]);
        assert_eq!(read_response.reply[6], 0x01);
        assert_eq!(&read_response.reply[7..9], &[0x00, 0x20]);
        assert_eq!(&read_response.reply[9..9 + barcode.len()], barcode);
        let _ = write_shared_loaded_cartridge("drive-attr-write", None);
    }

    #[test]
    fn test_drive_write_attribute_persists_across_state_reload() {
        let layout = temp_layout_for_test("mam-write-persist");
        let barcode = b"PERSIST-BC01";

        let mut state = crate::scsi_tape::state::TapeState::new("drive-attr-persist");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        state.cartridge_id = Some("VTLWATTR02".to_string());
        state.active_layout = Some(layout.clone());
        write_shared_loaded_cartridge("drive-attr-persist", Some("VTLWATTR02"))
            .expect("write shared state");

        let mut payload = vec![0x08, 0x06, 0x01, 0x00, barcode.len() as u8];
        payload.extend_from_slice(barcode);
        let mut write_cdb = vec![0x8D, 0x00, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0];
        write_cdb[10..14].copy_from_slice(&(payload.len() as u32).to_be_bytes());
        let write_response = dispatch_raw_cdb(&mut state, &write_cdb, &payload);
        assert_eq!(
            write_response.status, SCSI_STATUS_GOOD,
            "sense={:?}",
            write_response.sense
        );
        drain_unit_attention(&mut state);

        let mut reloaded = crate::scsi_tape::state::TapeState::new("drive-attr-persist");
        reloaded.mount_state = crate::scsi_tape::state::MountState::Loaded;
        reloaded.cartridge_id = Some("VTLWATTR02".to_string());
        reloaded.active_layout = Some(layout);

        let read_cdb = vec![
            0x8C, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // op/service + partition
            0x08, 0x06, // first attribute
            0x00, 0x00, 0x00, 0x80, // allocation length
            0x00, 0x00,
        ];
        let read_response = dispatch_raw_cdb(&mut reloaded, &read_cdb, &[]);
        assert_eq!(read_response.status, SCSI_STATUS_GOOD);
        assert_eq!(&read_response.reply[9..9 + barcode.len()], barcode);
        let _ = write_shared_loaded_cartridge("drive-attr-persist", None);
    }

    #[test]
    fn test_drive_write_attribute_accepts_parameter_list_header() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-attr-header");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        state.cartridge_id = Some("VTLWATTR04".to_string());
        state.active_layout = Some(temp_layout_for_test("mam-write-header"));
        write_shared_loaded_cartridge("drive-attr-header", Some("VTLWATTR04"))
            .expect("write shared state");

        let app_vendor = b"HOLO";
        let app_name = b"Backup";
        let app_version = b"1.0";
        let last_written = b"202604221036";
        let barcode = b"HEADER-BC01";
        let mut entries = Vec::new();
        for (attr, format, value) in [
            (0x0800u16, 0x01u8, app_vendor.as_slice()),
            (0x0801u16, 0x01u8, app_name.as_slice()),
            (0x0802u16, 0x01u8, app_version.as_slice()),
            (0x0804u16, 0x01u8, last_written.as_slice()),
            (0x0806u16, 0x01u8, barcode.as_slice()),
        ] {
            entries.extend_from_slice(&attr.to_be_bytes());
            entries.push(format);
            entries.extend_from_slice(&(value.len() as u16).to_be_bytes());
            entries.extend_from_slice(value);
        }
        let mut payload = (entries.len() as u32).to_be_bytes().to_vec();
        payload.extend_from_slice(&entries);

        let mut write_cdb = vec![0x8D, 0x00, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0];
        write_cdb[10..14].copy_from_slice(&(payload.len() as u32).to_be_bytes());
        let write_response = dispatch_raw_cdb(&mut state, &write_cdb, &payload);
        assert_eq!(
            write_response.status, SCSI_STATUS_GOOD,
            "sense={:?}",
            write_response.sense
        );
        drain_unit_attention(&mut state);

        let read_cdb = vec![
            0x8C, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // op/service + partition
            0x08, 0x06, // first attribute
            0x00, 0x00, 0x00, 0x80, // allocation length
            0x00, 0x00,
        ];
        let read_response = dispatch_raw_cdb(&mut state, &read_cdb, &[]);
        assert_eq!(read_response.status, SCSI_STATUS_GOOD);
        assert_eq!(&read_response.reply[4..6], &[0x08, 0x06]);
        assert_eq!(&read_response.reply[9..9 + barcode.len()], barcode);
        let _ = write_shared_loaded_cartridge("drive-attr-header", None);
    }

    #[test]
    fn test_drive_write_attribute_rejects_readonly_id() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-attr-readonly");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        state.cartridge_id = Some("VTLWATTR03".to_string());
        state.active_layout = Some(temp_layout_for_test("mam-write-readonly"));
        write_shared_loaded_cartridge("drive-attr-readonly", Some("VTLWATTR03"))
            .expect("write shared state");

        let payload = vec![
            0x00, 0x00, // remaining capacity (readonly)
            0x80, // binary readonly
            0x00, 0x08, 0, 0, 0, 0, 0, 0, 0, 1,
        ];
        let mut write_cdb = vec![0x8D, 0x00, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0];
        write_cdb[10..14].copy_from_slice(&(payload.len() as u32).to_be_bytes());
        let write_response = dispatch_raw_cdb(&mut state, &write_cdb, &payload);
        assert_eq!(write_response.status, SCSI_STATUS_CHECK_CONDITION);
        assert!(write_response.sense.len() >= 14);
        assert_eq!(write_response.sense[2] & 0x0F, 0x05);
        assert_eq!(write_response.sense[12], 0x26);
        let _ = write_shared_loaded_cartridge("drive-attr-readonly", None);
    }

    #[test]
    fn test_drive_write_attribute_rejects_oversized_parameter_list() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-attr-oversized");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        let oversized_len = (64 * 1024 + 1) as u32;
        let payload = vec![0u8; oversized_len as usize];
        let mut write_cdb = vec![0x8D, 0x00, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0];
        write_cdb[10..14].copy_from_slice(&oversized_len.to_be_bytes());

        let write_response = dispatch_raw_cdb(&mut state, &write_cdb, &payload);
        assert_eq!(write_response.status, SCSI_STATUS_CHECK_CONDITION);
        assert!(write_response.sense.len() >= 14);
        assert_eq!(write_response.sense[2] & 0x0F, 0x05);
        assert_eq!(write_response.sense[12], 0x24);
        assert_eq!(write_response.sense[13], 0x00);
    }

    #[test]
    fn test_drive_mode_select_6_accepts_empty_parameter_list() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-msel6");
        let cdb = vec![0x15, 0x10, 0x00, 0x00, 0x00, 0x00];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
    }

    #[test]
    fn test_drive_mode_select_10_accepts_payload() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-msel10");
        let cdb = vec![0x55, 0x10, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x08, 0x00];
        let payload = vec![0u8; 8];
        let response = dispatch_raw_cdb(&mut state, &cdb, &payload);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
    }

    #[test]
    fn test_drive_mode_select_10_updates_data_compression_enable() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-msel10-dce");
        assert!(state.data_compression_enabled);
        let payload = vec![
            0x00, 0x00, 0x00, 0x00, // mode data length + medium type
            0x00, 0x00, 0x00, 0x00, // device specific + block desc len
            0x0F, 0x0E, // data compression page
            0x40, // DCC only, DCE disabled
            0x80, // decompression enabled
            0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
        ];
        let cdb = vec![
            0x55,
            0x10,
            0x00,
            0x00,
            0x00,
            0x00,
            0x00,
            0x00,
            payload.len() as u8,
            0x00,
        ];
        let response = dispatch_raw_cdb(&mut state, &cdb, &payload);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert!(!state.data_compression_enabled);

        let mode_sense = dispatch_raw_cdb(
            &mut state,
            &[0x5A, 0x00, 0x0F, 0x00, 0x00, 0x00, 0x00, 0x00, 0x40, 0x00],
            &[],
        );
        assert_eq!(mode_sense.status, SCSI_STATUS_GOOD);
        let idx = mode_sense
            .reply
            .windows(2)
            .position(|pair| pair == [0x0F, 0x0E])
            .expect("missing data compression page");
        assert_eq!(mode_sense.reply[idx + 2], 0x40);
    }

    #[test]
    fn test_drive_mode_select_10_cannot_enable_disabled_compression_policy() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-msel10-dce-policy");
        state.data_compression_allowed = false;
        state.data_compression_enabled = false;
        let payload = vec![
            0x00, 0x00, 0x00, 0x00, // mode data length + medium type
            0x00, 0x00, 0x00, 0x00, // device specific + block desc len
            0x0F, 0x0E, // data compression page
            0xC0, // DCC + DCE requested
            0x80, // decompression enabled
            0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
        ];
        let cdb = vec![
            0x55,
            0x10,
            0x00,
            0x00,
            0x00,
            0x00,
            0x00,
            0x00,
            payload.len() as u8,
            0x00,
        ];

        let response = dispatch_raw_cdb(&mut state, &cdb, &payload);

        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert!(!state.data_compression_enabled);
    }

    #[test]
    fn test_drive_mode_select_10_updates_medium_partition_runtime() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-msel10-part");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        let payload = vec![
            0x00, 0x00, 0x00, 0x00, // mode data length + medium type
            0x00, 0x00, 0x00, 0x00, // device specific + block desc len
            0x11, 0x0E, // medium partition page
            0x03, 0x01, // max addl, addl defined
            0x3C, 0x03, // fdp (IDP), medium fmt recognition
            0x09, 0x00, // partition units, reserved
            0x00, 0x64, // partition 0 units
            0x00, 0x10, // partition 1 units
            0x00, 0x00, // partition 2 units
            0x00, 0x00, // partition 3 units
        ];
        let cdb = vec![
            0x55,
            0x10,
            0x00,
            0x00,
            0x00,
            0x00,
            0x00,
            0x00,
            payload.len() as u8,
            0x00,
        ];
        let response = dispatch_raw_cdb(&mut state, &cdb, &payload);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert_eq!(state.partition_runtime.addl_partitions_defined, 1);
        assert_eq!(state.partition_runtime.partition_units, 9);
        assert_eq!(state.partition_runtime.partition_size_units[0], 0x0064);
        assert_eq!(state.partition_runtime.partition_size_units[1], 0x0010);
    }

    #[test]
    fn test_drive_mode_select_6_updates_buffered_and_block_mode() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-msel6-update");
        let _ = state.take_unit_attention();
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        let cdb = vec![0x15, 0x10, 0x00, 0x00, 0x0C, 0x00];
        // Header(4) + block descriptor(8); device specific parameter disables buffered mode.
        let payload = vec![
            0x00, 0x00, 0x00, 0x08, // mode header 6
            0x00, 0x00, 0x00, 0x00, // block descriptor
            0x00, 0x00, 0x02, 0x00, // block length = 512
        ];
        let response = dispatch_raw_cdb(&mut state, &cdb, &payload);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert!(!state.buffered_mode);
        assert_eq!(
            state.block_mode.mode,
            crate::scsi_tape::state::BlockMode::Fixed
        );
        assert_eq!(state.block_mode.fixed_block_size, 512);

        let tur = dispatch_raw_cdb(&mut state, &[0x00, 0x00, 0x00, 0x00, 0x00, 0x00], &[]);
        assert_eq!(tur.status, SCSI_STATUS_GOOD);
    }

    #[test]
    fn test_drive_persistent_reserve_out_updates_reservation_state() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-prout");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;

        let reservation_key = 0x1122_3344_5566_7788u64;

        let mut register_params = vec![0u8; 24];
        register_params[8..16].copy_from_slice(&reservation_key.to_be_bytes());
        let register_cdb = vec![0x5F, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x18, 0x00];
        let register_response = dispatch_raw_cdb(&mut state, &register_cdb, &register_params);
        assert_eq!(register_response.status, SCSI_STATUS_GOOD);

        assert_eq!(
            state
                .reservation_state
                .registrations
                .get(crate::scsi_tape::command_chain::default_initiator()),
            Some(&reservation_key)
        );

        let mut reserve_params = vec![0u8; 24];
        reserve_params[0..8].copy_from_slice(&reservation_key.to_be_bytes());
        let reserve_cdb = vec![0x5F, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x18, 0x00];
        let reserve_response = dispatch_raw_cdb(&mut state, &reserve_cdb, &reserve_params);
        assert_eq!(reserve_response.status, SCSI_STATUS_GOOD);
        assert_eq!(
            state.reservation_state.active_owner.as_deref(),
            Some(crate::scsi_tape::command_chain::default_initiator())
        );
        assert_eq!(state.reservation_state.active_key, Some(reservation_key));

        let mut clear_params = vec![0u8; 24];
        clear_params[0..8].copy_from_slice(&reservation_key.to_be_bytes());
        let clear_cdb = vec![0x5F, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x18, 0x00];
        let clear_response = dispatch_raw_cdb(&mut state, &clear_cdb, &clear_params);
        assert_eq!(clear_response.status, SCSI_STATUS_GOOD);
        assert!(state.reservation_state.registrations.is_empty());
        assert!(state.reservation_state.active_owner.is_none());
        assert!(state.reservation_state.active_key.is_none());
    }

    #[test]
    fn test_drive_persistent_reserve_out_uses_dispatch_initiator_context() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-prout-context");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;

        let key_a = 0x1111_2222_3333_4444u64;
        let key_b = 0x5555_6666_7777_8888u64;
        let mut register_a = vec![0u8; 24];
        register_a[8..16].copy_from_slice(&key_a.to_be_bytes());
        let mut register_b = vec![0u8; 24];
        register_b[8..16].copy_from_slice(&key_b.to_be_bytes());
        let register_cdb = vec![0x5F, 0x00, 0, 0, 0, 0, 0, 0, 0x18, 0];

        let response_a = dispatch_raw_cdb_with_context(
            &mut state,
            &register_cdb,
            &register_a,
            CdbDispatchContext::with_initiator("host-a"),
        );
        assert_eq!(response_a.status, SCSI_STATUS_GOOD);
        let response_b = dispatch_raw_cdb_with_context(
            &mut state,
            &register_cdb,
            &register_b,
            CdbDispatchContext::with_initiator("host-b"),
        );
        assert_eq!(response_b.status, SCSI_STATUS_GOOD);

        assert_eq!(
            state.reservation_state.registrations.get("host-a"),
            Some(&key_a)
        );
        assert_eq!(
            state.reservation_state.registrations.get("host-b"),
            Some(&key_b)
        );

        let mut reserve_a = vec![0u8; 24];
        reserve_a[0..8].copy_from_slice(&key_a.to_be_bytes());
        let reserve_cdb = vec![0x5F, 0x01, 0, 0, 0, 0, 0, 0, 0x18, 0];
        let reserve_response = dispatch_raw_cdb_with_context(
            &mut state,
            &reserve_cdb,
            &reserve_a,
            CdbDispatchContext::with_initiator("host-a"),
        );
        assert_eq!(reserve_response.status, SCSI_STATUS_GOOD);

        let write_response = dispatch_raw_cdb_with_context(
            &mut state,
            &[0x0A, 0, 0, 0, 1, 0],
            &[0xAA],
            CdbDispatchContext::with_initiator("host-b"),
        );
        assert_eq!(write_response.status, SCSI_STATUS_CHECK_CONDITION);
        assert_eq!(write_response.sense[2] & 0x0F, 0x0B);
        assert_eq!(write_response.sense[12], 0x18);
    }

    #[test]
    fn test_drive_persistent_reserve_in_reports_registered_keys_and_reservation() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-prin");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        state.reservation_state.generation = 3;
        state
            .reservation_state
            .registrations
            .insert("host-a".to_string(), 0x1111_2222_3333_4444);
        state
            .reservation_state
            .registrations
            .insert("host-b".to_string(), 0x5555_6666_7777_8888);
        state.reservation_state.active_owner = Some("host-b".to_string());
        state.reservation_state.active_key = Some(0x5555_6666_7777_8888);

        let read_keys_cdb = vec![0x5E, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x18, 0x00];
        let keys_response = dispatch_raw_cdb(&mut state, &read_keys_cdb, &[]);
        assert_eq!(keys_response.status, SCSI_STATUS_GOOD);
        assert_eq!(&keys_response.reply[0..4], &3u32.to_be_bytes());
        assert_eq!(&keys_response.reply[4..8], &16u32.to_be_bytes());
        assert_eq!(
            &keys_response.reply[8..16],
            &0x1111_2222_3333_4444u64.to_be_bytes()
        );
        assert_eq!(
            &keys_response.reply[16..24],
            &0x5555_6666_7777_8888u64.to_be_bytes()
        );

        let read_reservation_cdb = vec![0x5E, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x18, 0x00];
        let reservation_response = dispatch_raw_cdb(&mut state, &read_reservation_cdb, &[]);
        assert_eq!(reservation_response.status, SCSI_STATUS_GOOD);
        assert_eq!(&reservation_response.reply[0..4], &3u32.to_be_bytes());
        assert_eq!(&reservation_response.reply[4..8], &16u32.to_be_bytes());
        assert_eq!(
            &reservation_response.reply[8..16],
            &0x5555_6666_7777_8888u64.to_be_bytes()
        );
        assert_eq!(reservation_response.reply[21], 0x03);
    }

    #[test]
    fn test_drive_persistent_reserve_out_rejects_unknown_service_action() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-prout-invalid");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;

        let cdb = vec![0x5F, 0x1F, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x18, 0x00];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[0u8; 24]);
        assert_eq!(response.status, SCSI_STATUS_CHECK_CONDITION);
        assert!(response.sense.len() >= 14);
        assert_eq!(response.sense[2] & 0x0F, 0x05);
        assert_eq!(response.sense[12], 0x24);
        assert_eq!(response.sense[13], 0x00);
    }

    #[test]
    fn test_drive_persistent_reserve_out_register_and_move_moves_reservation() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-prout-register-move");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        let key_a = 0x1111_2222_3333_4444u64;
        let key_b = 0x5555_6666_7777_8888u64;

        let mut register_a = vec![0u8; 24];
        register_a[8..16].copy_from_slice(&key_a.to_be_bytes());
        let register_cdb = vec![0x5F, 0x00, 0, 0, 0, 0, 0, 0, 0x18, 0];
        let register_response = dispatch_raw_cdb_with_context(
            &mut state,
            &register_cdb,
            &register_a,
            CdbDispatchContext::with_initiator("iqn.1993-08.org.debian:host-a"),
        );
        assert_eq!(register_response.status, SCSI_STATUS_GOOD);

        let mut reserve_a = vec![0u8; 24];
        reserve_a[0..8].copy_from_slice(&key_a.to_be_bytes());
        let reserve_cdb = vec![0x5F, 0x01, 0, 0, 0, 0, 0, 0, 0x18, 0];
        let reserve_response = dispatch_raw_cdb_with_context(
            &mut state,
            &reserve_cdb,
            &reserve_a,
            CdbDispatchContext::with_initiator("iqn.1993-08.org.debian:host-a"),
        );
        assert_eq!(reserve_response.status, SCSI_STATUS_GOOD);

        let target = b"iqn.1993-08.org.debian:host-b";
        let mut move_params = vec![0u8; 24 + target.len() + 1];
        move_params[0..8].copy_from_slice(&key_a.to_be_bytes());
        move_params[8..16].copy_from_slice(&key_b.to_be_bytes());
        move_params[20] = 0x01;
        move_params[24..24 + target.len()].copy_from_slice(target);
        let mut move_cdb = vec![0x5F, 0x07, 0, 0, 0, 0, 0, 0, 0, 0];
        move_cdb[7..9].copy_from_slice(&(move_params.len() as u16).to_be_bytes());
        let move_response = dispatch_raw_cdb_with_context(
            &mut state,
            &move_cdb,
            &move_params,
            CdbDispatchContext::with_initiator("iqn.1993-08.org.debian:host-a"),
        );
        assert_eq!(
            move_response.status, SCSI_STATUS_GOOD,
            "sense={:?}",
            move_response.sense
        );

        assert!(!state
            .reservation_state
            .registrations
            .contains_key("iqn.1993-08.org.debian:host-a"));
        assert_eq!(
            state
                .reservation_state
                .registrations
                .get("iqn.1993-08.org.debian:host-b"),
            Some(&key_b)
        );
        assert_eq!(
            state.reservation_state.active_owner.as_deref(),
            Some("iqn.1993-08.org.debian:host-b")
        );
        assert_eq!(state.reservation_state.active_key, Some(key_b));
    }

    #[test]
    fn test_drive_persistent_reserve_out_register_and_move_rejects_missing_transport_id() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-prout-register-move-bad");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        let cdb = vec![0x5F, 0x07, 0, 0, 0, 0, 0, 0, 0x18, 0];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[0u8; 24]);
        assert_eq!(response.status, SCSI_STATUS_CHECK_CONDITION);
        assert_eq!(response.sense[2] & 0x0F, 0x05);
        assert_eq!(response.sense[12], 0x26);
        assert_eq!(response.sense[13], 0x00);
    }

    #[test]
    fn test_drive_format_medium_requires_bot() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-format-bot");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        state.current_position = 16;
        let cdb = vec![0x04, 0x00, 0x01, 0x00, 0x00, 0x00];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(response.status, SCSI_STATUS_CHECK_CONDITION);
        assert!(response.sense.len() >= 14);
        assert_eq!(response.sense[2] & 0x0F, 0x05);
        assert_eq!(response.sense[12], 0x3B);
        assert_eq!(response.sense[13], 0x0C);
    }

    #[test]
    fn test_drive_format_medium_applies_partition_capacity_for_overflow() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-format-part");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        let mode_select_payload = vec![
            0x00, 0x00, 0x00, 0x00, // mode data length + medium type
            0x00, 0x00, 0x00, 0x00, // device specific + block desc len
            0x11, 0x0E, // medium partition page
            0x03, 0x01, // max addl, addl defined
            0x3C, 0x03, // fdp (IDP), medium fmt recognition
            0x09, 0x00, // partition units, reserved
            0x00, 0x01, // partition 0 = 1 GiB
            0xFF, 0xFF, // partition 1 fill-to-max
            0x00, 0x00, // partition 2
            0x00, 0x00, // partition 3
        ];
        let mode_select_cdb = vec![
            0x55,
            0x10,
            0x00,
            0x00,
            0x00,
            0x00,
            0x00,
            0x00,
            mode_select_payload.len() as u8,
            0x00,
        ];
        let mode_select_resp = dispatch_raw_cdb(&mut state, &mode_select_cdb, &mode_select_payload);
        assert_eq!(mode_select_resp.status, SCSI_STATUS_GOOD);

        let format_cdb = vec![0x04, 0x00, 0x01, 0x00, 0x00, 0x00];
        let format_resp = dispatch_raw_cdb(&mut state, &format_cdb, &[]);
        assert_eq!(format_resp.status, SCSI_STATUS_GOOD);
        drain_unit_attention(&mut state);
        assert_eq!(
            state.partition_runtime.partition_sizes_bytes[0],
            1024 * 1024 * 1024
        );
        assert_eq!(state.current_position, 0);
        assert_eq!(state.eod_position, 0);

        state.current_position = state.partition_runtime.partition_sizes_bytes[0];
        let write_cdb = vec![0x0A, 0x00, 0x00, 0x00, 0x01, 0x00];
        let write_resp = dispatch_raw_cdb(&mut state, &write_cdb, &[0xAA]);
        assert_eq!(write_resp.status, SCSI_STATUS_CHECK_CONDITION);
        assert!(write_resp.sense.len() >= 14);
        assert_eq!(write_resp.sense[2] & 0x0F, 0x0D);
    }

    #[test]
    fn test_drive_write_reports_volume_overflow_with_eom() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-overflow");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        let profile = crate::scsi_tape::profiles::resolve_drive_profile_from_env();
        state.current_position = native_capacity_bytes_for_profile(&profile);

        let cdb = vec![0x0A, 0x00, 0x00, 0x00, 0x01, 0x00];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[0xAA]);
        assert_eq!(response.status, SCSI_STATUS_CHECK_CONDITION);
        assert!(response.sense.len() >= 14);
        assert_eq!(response.sense[2] & 0x0F, 0x0D);
        assert_eq!(response.sense[2] & 0x40, 0x40);
    }

    #[test]
    fn test_drive_write6_empty_probe_at_bot_returns_good() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-write6-probe");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        state.current_position = 0;
        state.eod_position = 0;
        state.block_mode.mode = crate::scsi_tape::state::BlockMode::Fixed;
        state.block_mode.fixed_block_size = 262_144;

        let cdb = vec![0x0A, 0x01, 0x00, 0x00, 0x01, 0x00];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert_eq!(state.current_position, 0);
        assert_eq!(state.eod_position, 0);
    }

    #[test]
    fn test_drive_write6_empty_probe_after_bot_still_rejected() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-write6-nonbot");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        state.current_position = 1;
        state.eod_position = 1;
        state.block_mode.mode = crate::scsi_tape::state::BlockMode::Fixed;
        state.block_mode.fixed_block_size = 262_144;

        let cdb = vec![0x0A, 0x01, 0x00, 0x00, 0x01, 0x00];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(response.status, SCSI_STATUS_CHECK_CONDITION);
        assert!(response.sense.len() >= 14);
        assert_eq!(response.sense[2] & 0x0F, 0x05);
        assert_eq!(response.sense[12], 0x24);
        assert_eq!(response.sense[13], 0x00);
    }

    #[test]
    fn test_drive_write6_single_byte_probe_at_bot_returns_good_without_advance() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-write6-probe-single-byte");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        state.current_position = 0;
        state.eod_position = 0;
        state.block_mode.mode = crate::scsi_tape::state::BlockMode::Fixed;
        state.block_mode.fixed_block_size = 262_144;

        let cdb = vec![0x0A, 0x01, 0x00, 0x00, 0x01, 0x00];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[0x00]);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert_eq!(state.current_position, 0);
        assert_eq!(state.eod_position, 0);

        let response_non_zero = dispatch_raw_cdb(&mut state, &cdb, &[0x54]);
        assert_eq!(response_non_zero.status, SCSI_STATUS_GOOD);
        assert_eq!(state.current_position, 0);
        assert_eq!(state.eod_position, 0);
    }

    #[test]
    fn test_drive_write6_fixed_multi_block_payload_succeeds() {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let mut state =
            crate::scsi_tape::state::TapeState::new(format!("drive-write6-fixed-multi-{nanos}"));
        crate::media::mount_bridge::attach_cartridge(&mut state, "VTAFIXED001").expect("attach");
        state.block_mode.mode = crate::scsi_tape::state::BlockMode::Fixed;
        state.block_mode.fixed_block_size = 262_144;

        let cdb = vec![0x0A, 0x01, 0x00, 0x00, 0x04, 0x00];
        let payload = vec![0x5A; 1_048_576];
        let response =
            dispatch_fixed_block_io(&mut state, &cdb, &payload).expect("fixed write should apply");
        assert_eq!(
            response.status, SCSI_STATUS_GOOD,
            "write6 multi-block sense={:?}",
            response.sense
        );
        assert_eq!(state.current_position, 1_048_576);
        assert_eq!(state.eod_position, 1_048_576);
    }

    #[test]
    fn test_drive_read6_fixed_multi_block_payload_succeeds() {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let mut state =
            crate::scsi_tape::state::TapeState::new(format!("drive-read6-fixed-multi-{nanos}"));
        crate::media::mount_bridge::attach_cartridge(&mut state, "VTAFIXED002").expect("attach");
        state.block_mode.mode = crate::scsi_tape::state::BlockMode::Fixed;
        state.block_mode.fixed_block_size = 262_144;

        let write_cdb = vec![0x0A, 0x01, 0x00, 0x00, 0x04, 0x00];
        let payload = vec![0x6B; 1_048_576];
        let write_response =
            dispatch_fixed_block_io(&mut state, &write_cdb, &payload).expect("fixed write apply");
        assert_eq!(
            write_response.status, SCSI_STATUS_GOOD,
            "read6 setup write sense={:?}",
            write_response.sense
        );
        state.current_position = 0;

        let read_cdb = vec![0x08, 0x01, 0x00, 0x00, 0x04, 0x00];
        let read_response =
            dispatch_fixed_block_io(&mut state, &read_cdb, &[]).expect("fixed read should apply");
        assert_eq!(read_response.status, SCSI_STATUS_GOOD);
        assert_eq!(read_response.reply.len(), 1_048_576);
        assert_eq!(read_response.reply[0], 0x6B);
        assert_eq!(read_response.reply[read_response.reply.len() - 1], 0x6B);
    }

    #[test]
    fn test_drive_write6_fixed_multi_block_length_mismatch_rejected() {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let mut state =
            crate::scsi_tape::state::TapeState::new(format!("drive-write6-fixed-mismatch-{nanos}"));
        crate::media::mount_bridge::attach_cartridge(&mut state, "VTAFIXED003").expect("attach");
        state.block_mode.mode = crate::scsi_tape::state::BlockMode::Fixed;
        state.block_mode.fixed_block_size = 262_144;

        let cdb = vec![0x0A, 0x01, 0x00, 0x00, 0x04, 0x00];
        let payload = vec![0x5A; 262_144];
        let response = dispatch_fixed_block_io(&mut state, &cdb, &payload)
            .expect("fixed write should be handled");
        assert_eq!(response.status, SCSI_STATUS_CHECK_CONDITION);
        assert!(response.sense.len() >= 14);
        assert_eq!(response.sense[12], 0x24);
        assert_eq!(response.sense[13], 0x00);
    }

    #[test]
    fn test_drive_log_sense_compression_page_reports_good() {
        let mut state = crate::scsi_tape::state::TapeState::new("drive-log32");
        let cdb = vec![0x4D, 0x00, 0x32, 0x00, 0x00, 0x00, 0x00, 0x00, 0x40, 0x00];
        let response = dispatch_raw_cdb(&mut state, &cdb, &[]);
        assert_eq!(response.status, SCSI_STATUS_GOOD);
        assert!(response.reply.len() >= 4);
        assert_eq!(response.reply[0] & 0x3F, 0x32);
    }

    #[test]
    fn drive_missing_command_set_returns_compatible_status() {
        let mut state = crate::scsi_tape::state::TapeState::new("compat-drive-cmds");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        state.cartridge_id = Some("VTL-DRIVE-001".to_string());
        write_shared_loaded_cartridge("compat-drive-cmds", Some("VTL-DRIVE-001"))
            .expect("write shared state");
        state.eod_position = 1024;
        state.current_position = 0;
        state.block_starts = vec![0, 512];
        state.block_lengths.insert(0, 512);
        state.block_lengths.insert(512, 512);

        let verify = dispatch_raw_cdb(&mut state, &[0x13, 0, 0, 0, 0, 0], &[]);
        assert_eq!(verify.status, SCSI_STATUS_GOOD);
        assert_eq!(state.current_position, 0);
        let verify_vbf = dispatch_raw_cdb(&mut state, &[0x13, 0x02, 0, 0, 0, 0], &[]);
        assert_eq!(verify_vbf.status, SCSI_STATUS_GOOD);
        assert_eq!(state.current_position, 0);

        let reserve6 = dispatch_raw_cdb(&mut state, &[0x16, 0, 0, 0, 0, 0], &[]);
        assert_eq!(reserve6.status, SCSI_STATUS_GOOD);
        let release6 = dispatch_raw_cdb(&mut state, &[0x17, 0, 0, 0, 0, 0], &[]);
        assert_eq!(release6.status, SCSI_STATUS_GOOD);
        let reserve10 = dispatch_raw_cdb(&mut state, &[0x56, 0, 0, 0, 0, 0, 0, 0, 0, 0], &[]);
        assert_eq!(reserve10.status, SCSI_STATUS_GOOD);
        let release10 = dispatch_raw_cdb(&mut state, &[0x57, 0, 0, 0, 0, 0, 0, 0, 0, 0], &[]);
        assert_eq!(release10.status, SCSI_STATUS_GOOD);

        let mut space16_cdb = [0u8; 16];
        space16_cdb[0] = 0x91;
        space16_cdb[1] = 0x00;
        space16_cdb[11] = 0x01;
        let space16 = dispatch_raw_cdb(&mut state, &space16_cdb, &[]);
        assert_eq!(space16.status, SCSI_STATUS_GOOD);
        assert_eq!(state.current_position, 512);

        let mut read_serial = [0u8; 12];
        read_serial[0] = 0xAB;
        read_serial[9] = 0x40;
        let serial = dispatch_raw_cdb(&mut state, &read_serial, &[]);
        assert_eq!(serial.status, SCSI_STATUS_GOOD);
        assert!(serial.reply.len() >= 4);
        let serial_len = u32::from_be_bytes([
            serial.reply[0],
            serial.reply[1],
            serial.reply[2],
            serial.reply[3],
        ]) as usize;
        assert_eq!(serial_len, "VTLDRIVE001".len());
        assert_eq!(&serial.reply[4..4 + serial_len], b"VTLDRIVE001");
        let _ = write_shared_loaded_cartridge("compat-drive-cmds", None);
    }

    #[test]
    fn worm_reserve_allow_overwrite_stays_compatible_on_locked_worm_media() {
        let mut state = crate::scsi_tape::state::TapeState::new("compat-overwrite-worm");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        state.retention_policy.is_worm_media = true;
        state.retention_policy.retention_locked = true;

        let allow = dispatch_raw_cdb(
            &mut state,
            &[0x82, 0, 0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0],
            &[],
        );
        assert_eq!(allow.status, SCSI_STATUS_GOOD);
        assert_eq!(
            state.allow_overwrite,
            Some(crate::scsi_tape::state::AllowOverwriteState {
                allow_type: 1,
                partition: 0,
                block_address: 0,
            })
        );

        let write = dispatch_raw_cdb(&mut state, &[0x0A, 0, 0, 0, 1, 0], b"x");
        assert_eq!(write.status, SCSI_STATUS_CHECK_CONDITION);
        assert_eq!(write.sense[2] & 0x0F, 0x07);
        assert_eq!(write.sense[12], 0x30);
        assert_eq!(write.sense[13], 0x0C);
    }

    #[test]
    fn worm_reserve_allow_overwrite_stays_compatible_on_written_worm_media_but_allows_clear() {
        let mut state = crate::scsi_tape::state::TapeState::new("compat-overwrite-worm-written");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        state.retention_policy.is_worm_media = true;
        state.eod_position = 1;
        state.allow_overwrite = Some(crate::scsi_tape::state::AllowOverwriteState {
            allow_type: 1,
            partition: 0,
            block_address: 0,
        });

        let allow = dispatch_raw_cdb(
            &mut state,
            &[0x82, 0, 0x02, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0],
            &[],
        );
        assert_eq!(allow.status, SCSI_STATUS_GOOD);
        assert_eq!(
            state.allow_overwrite,
            Some(crate::scsi_tape::state::AllowOverwriteState {
                allow_type: 2,
                partition: 0,
                block_address: 0,
            })
        );

        let write = dispatch_raw_cdb(&mut state, &[0x0A, 0, 0, 0, 1, 0], b"x");
        assert_eq!(write.status, SCSI_STATUS_CHECK_CONDITION);
        assert_eq!(write.sense[2] & 0x0F, 0x07);
        assert_eq!(write.sense[12], 0x30);
        assert_eq!(write.sense[13], 0x0C);

        let clear = dispatch_raw_cdb(
            &mut state,
            &[0x82, 0, 0x00, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0],
            &[],
        );
        assert_eq!(clear.status, SCSI_STATUS_GOOD);
        assert_eq!(state.allow_overwrite, None);
    }

    #[test]
    fn changer_rezero_and_import_export_open_close() {
        let mut state = crate::scsi_tape::state::TapeState::new("compat-changer-cmds");
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");
        state.changer_slots.insert(0x0400, None);
        state
            .changer_slots
            .insert(0x0401, Some("CUSTOM01".to_string()));
        state.changer_ie_impexp.insert(0x0300, false);

        let rezero = dispatch_changer_cdb(&mut state, &[0x01, 0, 0, 0, 0, 0], &[], profile.clone());
        assert_eq!(rezero.status, SCSI_STATUS_GOOD);
        assert!(state.changer_slots.contains_key(&0x0400));
        assert_eq!(
            state.changer_slots.get(&0x0401),
            Some(&Some("CUSTOM01".to_string()))
        );

        let open = dispatch_changer_cdb(
            &mut state,
            &[0x1B, 0, 0x03, 0x00, 0x01, 0],
            &[],
            profile.clone(),
        );
        assert_eq!(open.status, SCSI_STATUS_GOOD);
        assert_eq!(state.changer_ie_impexp.get(&0x0300), Some(&true));

        let close = dispatch_changer_cdb(&mut state, &[0x1B, 0, 0x03, 0x00, 0x00, 0], &[], profile);
        assert_eq!(close.status, SCSI_STATUS_GOOD);
        assert_eq!(state.changer_ie_impexp.get(&0x0300), Some(&false));
    }

    #[test]
    fn worm_reserve_reserve_release_default_compat_good_for_changer() {
        let mut state = crate::scsi_tape::state::TapeState::new("compat-changer-reserve-release");
        let profile = crate::scsi_tape::profiles::resolve_changer_profile("ibm-03584l32");

        let reserve6 =
            dispatch_changer_cdb(&mut state, &[0x16, 0, 0, 0, 0, 0], &[], profile.clone());
        assert_eq!(reserve6.status, SCSI_STATUS_GOOD);
        let release6 = dispatch_changer_cdb(&mut state, &[0x17, 0, 0, 0, 0, 0], &[], profile);
        assert_eq!(release6.status, SCSI_STATUS_GOOD);
    }

    #[test]
    fn mode_log_pages_include_health_and_state_backed_counters() {
        let mut state = crate::scsi_tape::state::TapeState::new("compat-pages");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        state.eod_position = 2 * 1024 * 1024;
        state.filemarks = vec![1024];
        state.usage_counters.lifetime_write_ops = 2;
        state.usage_counters.lifetime_read_ops = 1;
        state.usage_counters.lifetime_filemark_ops = 1;
        state.usage_counters.lifetime_bytes_written = 2 * 1024 * 1024;
        state.usage_counters.lifetime_bytes_read = 1024 * 1024;
        state.usage_counters.last_load_write_ops = 2;
        state.usage_counters.last_load_read_ops = 1;
        state.usage_counters.last_load_filemark_ops = 1;
        state.usage_counters.last_load_bytes_written = 2 * 1024 * 1024;
        state.usage_counters.last_load_bytes_read = 1024 * 1024;

        let mode = dispatch_raw_cdb(&mut state, &[0x1A, 0x08, 0x1C, 0, 0x40, 0], &[]);
        assert_eq!(mode.status, SCSI_STATUS_GOOD);
        assert!(mode.reply.windows(2).any(|pair| pair == [0x1C, 0x0A]));

        let supported =
            dispatch_raw_cdb(&mut state, &[0x4D, 0x00, 0x00, 0, 0, 0, 0, 0, 0x40, 0], &[]);
        assert_eq!(supported.status, SCSI_STATUS_GOOD);
        for page in [0x02u8, 0x03, 0x0C, 0x17, 0x31, 0x32, 0x37] {
            assert!(
                supported.reply.contains(&page),
                "supported log pages missing 0x{page:02X}"
            );
        }

        let sequential =
            dispatch_raw_cdb(&mut state, &[0x4D, 0x00, 0x0C, 0, 0, 0, 0, 0, 0x80, 0], &[]);
        assert_eq!(sequential.status, SCSI_STATUS_GOOD);
        assert_eq!(log_param_u32(&sequential.reply, 0x0001), Some(2));
        assert_eq!(log_param_u32(&sequential.reply, 0x0002), Some(1));
        assert_eq!(log_param_u32(&sequential.reply, 0x0003), Some(1));
        assert_eq!(log_param_u32(&sequential.reply, 0x0004), None);
        assert_eq!(log_param_u32(&sequential.reply, 0x0005), None);

        let performance =
            dispatch_raw_cdb(&mut state, &[0x4D, 0x00, 0x37, 0, 0, 0, 0, 0, 0x40, 0], &[]);
        assert_eq!(performance.status, SCSI_STATUS_GOOD);
        assert_eq!(log_param_u32(&performance.reply, 0x0001), Some(160));
    }

    #[test]
    fn log_page_0c_does_not_report_initialized_eod_as_capacity() {
        let mut state = crate::scsi_tape::state::TapeState::new("capacity-regression");
        state.mount_state = crate::scsi_tape::state::MountState::Loaded;
        state.eod_position = 1024 * 1024;
        state.block_mode.fixed_block_size = 256 * 1024;
        state.usage_counters.load_count = 7;
        state.usage_counters.last_load_write_ops = 4;
        state.usage_counters.last_load_bytes_written = 1024 * 1024;

        let sequential =
            dispatch_raw_cdb(&mut state, &[0x4D, 0x00, 0x0C, 0, 0, 0, 0, 0, 0x80, 0], &[]);
        assert_eq!(sequential.status, SCSI_STATUS_GOOD);
        assert_eq!(log_param_u32(&sequential.reply, 0x0004), None);
        assert_eq!(log_param_u32(&sequential.reply, 0x0005), None);

        let capacity =
            dispatch_raw_cdb(&mut state, &[0x4D, 0x00, 0x31, 0, 0, 0, 0, 0, 0x40, 0], &[]);
        assert_eq!(capacity.status, SCSI_STATUS_GOOD);
        assert_eq!(log_param_u32(&capacity.reply, 0x0002), Some(0));
        assert!(log_param_u32(&capacity.reply, 0x0003).unwrap_or(0) > 1024);

        let volume = dispatch_raw_cdb(&mut state, &[0x4D, 0x00, 0x17, 0, 0, 0, 0, 0, 0xC0, 0], &[]);
        assert_eq!(volume.status, SCSI_STATUS_GOOD);
        assert_eq!(log_param_u32(&volume.reply, 0x0001), Some(7));
        assert_eq!(log_partition_param_u32(&volume.reply, 0x0203, 0), Some(1));
        assert!(log_partition_param_u32(&volume.reply, 0x0202, 0).unwrap_or(0) > 1024);
    }

    #[test]
    fn verify_reads_and_compares_payload() {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let drive_id = format!("verify-{nanos}");
        let cartridge_id = format!("VRFY{nanos}");
        let mut state = crate::scsi_tape::state::TapeState::new(drive_id.clone());
        crate::media::mount_bridge::attach_cartridge(&mut state, &cartridge_id).expect("attach");
        write_shared_loaded_cartridge(&drive_id, Some(&cartridge_id)).expect("write shared state");
        drain_unit_attention(&mut state);
        state.block_mode.mode = crate::scsi_tape::state::BlockMode::Variable;
        state.block_mode.fixed_block_size = 0;
        let payload = b"verify-payload".to_vec();

        let write = dispatch_raw_cdb(&mut state, &[0x0A, 0, 0, 0, 1, 0], &payload);
        assert_eq!(write.status, SCSI_STATUS_GOOD, "sense={:?}", write.sense);
        let rewind = dispatch_raw_cdb(&mut state, &[0x01, 0, 0, 0, 0, 0], &[]);
        assert_eq!(rewind.status, SCSI_STATUS_GOOD);

        let verify = dispatch_raw_cdb(&mut state, &[0x13, 0x04, 0, 0, 1, 0], &payload);
        assert_eq!(verify.status, SCSI_STATUS_GOOD, "sense={:?}", verify.sense);
        assert_eq!(state.current_position, payload.len() as u64);
        assert_eq!(state.usage_counters.last_load_read_ops, 1);

        let rewind = dispatch_raw_cdb(&mut state, &[0x01, 0, 0, 0, 0, 0], &[]);
        assert_eq!(rewind.status, SCSI_STATUS_GOOD);
        let miscompare = dispatch_raw_cdb(&mut state, &[0x13, 0x04, 0, 0, 1, 0], b"Verify-payload");
        assert_eq!(miscompare.status, SCSI_STATUS_CHECK_CONDITION);
        assert_eq!(miscompare.sense[2] & 0x0F, 0x0E);

        crate::media::mount_bridge::detach_cartridge(&mut state);
        let _ = write_shared_loaded_cartridge(&drive_id, None);
    }

    #[test]
    fn mam_usage_counters_move_and_persist_across_remount() {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("clock")
            .as_nanos();
        let drive_id = format!("compat-mam-{nanos}");
        let cartridge_id = format!("MAM{nanos}");
        let mut state = crate::scsi_tape::state::TapeState::new(drive_id.clone());
        crate::media::mount_bridge::attach_cartridge(&mut state, &cartridge_id).expect("attach");
        write_shared_loaded_cartridge(&drive_id, Some(&cartridge_id)).expect("write shared state");
        drain_unit_attention(&mut state);
        state.block_mode.mode = crate::scsi_tape::state::BlockMode::Variable;
        state.block_mode.fixed_block_size = 0;

        assert_eq!(attr_u64(&mut state, 0x0003), 1);
        let payload = vec![0x5A; 1024 * 1024];
        let write = dispatch_raw_cdb(&mut state, &[0x0A, 0, 0, 0, 1, 0], &payload);
        assert_eq!(write.status, SCSI_STATUS_GOOD, "sense={:?}", write.sense);
        let usage_path = state.usage_counter_path().expect("usage counter path");
        let before_unload = fs::read_to_string(&usage_path).expect("usage counters before unload");
        assert!(
            before_unload.contains("lifetime_write_ops=0"),
            "usage counters should not persist every write: {before_unload}"
        );

        let rewind = dispatch_raw_cdb(&mut state, &[0x01, 0, 0, 0, 0, 0], &[]);
        assert_eq!(rewind.status, SCSI_STATUS_GOOD);
        let read = dispatch_raw_cdb(&mut state, &[0x08, 0, 0, 0, 1, 0], &[]);
        assert_eq!(read.status, SCSI_STATUS_GOOD, "sense={:?}", read.sense);
        assert_eq!(read.reply.len(), payload.len());

        assert_eq!(attr_u64(&mut state, 0x0220), 1);
        assert_eq!(attr_u64(&mut state, 0x0221), 1);
        assert_eq!(attr_u64(&mut state, 0x0222), 1);
        assert_eq!(attr_u64(&mut state, 0x0223), 1);

        crate::media::mount_bridge::detach_cartridge(&mut state);
        let after_unload = fs::read_to_string(&usage_path).expect("usage counters after unload");
        assert!(after_unload.contains("lifetime_write_ops=1"));
        assert!(after_unload.contains("lifetime_read_ops=1"));

        let mut reloaded = crate::scsi_tape::state::TapeState::new(drive_id);
        crate::media::mount_bridge::attach_cartridge(&mut reloaded, &cartridge_id)
            .expect("reattach");
        write_shared_loaded_cartridge(&reloaded.drive_id, Some(&cartridge_id))
            .expect("write reloaded shared state");
        drain_unit_attention(&mut reloaded);
        assert_eq!(attr_u64(&mut reloaded, 0x0003), 2);
        assert_eq!(attr_u64(&mut reloaded, 0x0220), 1);
        assert_eq!(attr_u64(&mut reloaded, 0x0221), 1);
        assert_eq!(attr_u64(&mut reloaded, 0x0222), 0);
        assert_eq!(attr_u64(&mut reloaded, 0x0223), 0);
        let _ = write_shared_loaded_cartridge(&reloaded.drive_id, None);
    }
}
