use super::compression::{
    compress_payload, decompress_payload, CompressionCodec, MAX_DECOMPRESSED_PAYLOAD_LEN,
};

#[test]
fn compresses_and_roundtrips_rle_payload() {
    let input = b"AAAAAAAAAABBBBBBBBBBCCCCCCCCCC";
    let (codec, encoded, compressed) =
        compress_payload(CompressionCodec::Rle, input).expect("compress should pass");

    assert_eq!(codec, CompressionCodec::Rle);
    assert!(compressed);
    assert!(encoded.len() < input.len());

    let decoded = decompress_payload(codec, &encoded, input.len()).expect("decompress should pass");
    assert_eq!(decoded, input);
}

#[test]
fn falls_back_when_compression_not_beneficial() {
    let input = b"0123456789";
    let (codec, encoded, compressed) =
        compress_payload(CompressionCodec::Rle, input).expect("compress should pass");

    assert_eq!(codec, CompressionCodec::None);
    assert!(!compressed);
    assert_eq!(encoded, input);

    let decoded = decompress_payload(codec, &encoded, input.len()).expect("decompress should pass");
    assert_eq!(decoded, input);
}

#[test]
fn rejects_malformed_rle_payload() {
    let err = decompress_payload(CompressionCodec::Rle, &[3, b'A', 0, b'B'], 3)
        .expect_err("malformed rle should fail");
    assert!(format!("{err}").contains("rle"));
}

#[test]
fn rejects_oversized_lz4_payload_before_decode() {
    let declared_len = (MAX_DECOMPRESSED_PAYLOAD_LEN + 1) as u32;
    let mut payload = declared_len.to_le_bytes().to_vec();
    payload.extend_from_slice(b"not-enough-lz4-data");

    let err = decompress_payload(
        CompressionCodec::Lz4,
        &payload,
        MAX_DECOMPRESSED_PAYLOAD_LEN + 1,
    )
    .expect_err("oversized lz4 payload should fail before allocation");

    assert!(format!("{err}").contains("exceeds maximum"));
}

#[test]
fn rejects_oversized_zlib_expected_length_before_decode() {
    let payload = [0x78, 0x9c, 0x03, 0x00, 0x00, 0x00, 0x00, 0x01];
    let err = decompress_payload(
        CompressionCodec::Zlib,
        &payload,
        MAX_DECOMPRESSED_PAYLOAD_LEN + 1,
    )
    .expect_err("oversized zlib payload should fail before allocation");

    assert!(format!("{err}").contains("exceeds maximum"));
}

#[test]
fn lz4_roundtrip_for_repetitive_payload() {
    let input = vec![b'A'; 4096];
    let (codec, encoded, compressed) =
        compress_payload(CompressionCodec::Lz4, &input).expect("compress should pass");

    assert_eq!(codec, CompressionCodec::Lz4);
    assert!(compressed);
    assert!(encoded.len() < input.len());

    let decoded = decompress_payload(codec, &encoded, input.len()).expect("decompress should pass");
    assert_eq!(decoded, input);
}

#[test]
fn zlib_roundtrip_for_repetitive_payload() {
    let input = vec![b'Z'; 4096];
    let (codec, encoded, compressed) =
        compress_payload(CompressionCodec::Zlib, &input).expect("compress should pass");

    assert_eq!(codec, CompressionCodec::Zlib);
    assert!(compressed);
    assert!(encoded.len() < input.len());

    let decoded = decompress_payload(codec, &encoded, input.len()).expect("decompress should pass");
    assert_eq!(decoded, input);
}

#[test]
fn roundtrips_mixed_payload_patterns() {
    let input = (0..4096)
        .map(|i| {
            if i % 17 < 11 {
                0
            } else {
                b'A' + (i % 23) as u8
            }
        })
        .collect::<Vec<_>>();

    for requested in [CompressionCodec::Lz4, CompressionCodec::Zlib] {
        let (codec, encoded, compressed) =
            compress_payload(requested, &input).expect("compress should pass");

        assert_eq!(codec, requested);
        assert!(compressed);
        assert!(encoded.len() < input.len());
        let decoded =
            decompress_payload(codec, &encoded, input.len()).expect("decompress should pass");
        assert_eq!(decoded, input);
    }
}

#[test]
fn lz4_bypasses_when_payload_expands() {
    let input = b"tiny";
    let (codec, encoded, compressed) =
        compress_payload(CompressionCodec::Lz4, input).expect("compress should pass");

    assert_eq!(codec, CompressionCodec::None);
    assert!(!compressed);
    assert_eq!(encoded, input);
}

#[test]
fn zlib_bypasses_when_payload_expands() {
    let input = b"tiny";
    let (codec, encoded, compressed) =
        compress_payload(CompressionCodec::Zlib, input).expect("compress should pass");

    assert_eq!(codec, CompressionCodec::None);
    assert!(!compressed);
    assert_eq!(encoded, input);
}

#[test]
fn codec_from_u8_accepts_all_supported_variants() {
    assert_eq!(
        CompressionCodec::from_u8(0).unwrap(),
        CompressionCodec::None
    );
    assert_eq!(CompressionCodec::from_u8(1).unwrap(), CompressionCodec::Rle);
    assert_eq!(CompressionCodec::from_u8(2).unwrap(), CompressionCodec::Lz4);
    assert_eq!(
        CompressionCodec::from_u8(3).unwrap(),
        CompressionCodec::Zlib
    );
    assert!(CompressionCodec::from_u8(4).is_err());
}

#[test]
fn compression_stats_track_per_codec_writes() {
    let mut stats = super::compression::CompressionStats::default();
    stats.record_write(100, 50, CompressionCodec::Lz4, true);
    stats.record_write(100, 40, CompressionCodec::Zlib, true);
    stats.record_write(100, 100, CompressionCodec::None, false);

    assert_eq!(stats.compressed_writes, 2);
    assert_eq!(stats.lz4_writes, 1);
    assert_eq!(stats.zlib_writes, 1);
    assert_eq!(stats.bypassed_writes, 1);
}
