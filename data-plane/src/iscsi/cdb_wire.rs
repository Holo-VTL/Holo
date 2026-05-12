use std::io::{self, Read, Write};

/// TCMU CDB IPC wire protocol for holo.
///
/// Request frame (CdbPacket): sent from TCMU handler to data-plane
///   [u8  cdb_len ]  number of CDB bytes (6, 10, 12, or 16)
///   [N   cdb     ]  raw CDB bytes
///   [u32 data_len]  big-endian, number of data-out bytes (0 if none)
///   [M   data    ]  data-out payload (M = data_len)
///
/// Response frame (CdbResponse): sent from data-plane back to TCMU handler
///   [u8  sense_len]  0 = GOOD, >0 = sense data follows
///   [S   sense    ]  sense bytes (S = sense_len, max 252)
///   [u32 reply_len]  big-endian, number of data-in bytes (0 if none)
///   [R   reply    ]  data-in payload (R = reply_len)
///   [u8  status   ]  SCSI status: 0x00 GOOD, 0x02 CHECK CONDITION, 0x08 BUSY
pub const SCSI_STATUS_GOOD: u8 = 0x00;
pub const SCSI_STATUS_CHECK_CONDITION: u8 = 0x02;
pub const SCSI_STATUS_BUSY: u8 = 0x08;

/// Maximum sense buffer size (fixed-format sense descriptor, SPC-4 §4.5).
pub const MAX_SENSE_LEN: usize = 252;
/// Maximum inline data buffer transferred per CDB over the IPC socket.
pub const MAX_DATA_LEN: usize = 16 << 20; // 16 MiB
const EXTENDED_REQUEST_MARKER: u8 = 0xFF;
const EXTENDED_REQUEST_VERSION: u8 = 0x01;
const MAX_INITIATOR_LEN: usize = 255;

// ---------------------------------------------------------------------------
// CdbPacket — request frame
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CdbPacketHeader {
    /// Raw CDB bytes (6-16 bytes depending on command group).
    pub cdb: Vec<u8>,
    /// Data-out payload length following this header.
    pub data_len: usize,
    /// Optional transport-supplied SCSI initiator identity.
    pub initiator: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CdbPacket {
    /// Raw CDB bytes (6–16 bytes depending on command group).
    pub cdb: Vec<u8>,
    /// Data-out payload (WRITE commands etc.; empty for read/control commands).
    pub data: Vec<u8>,
    /// Optional transport-supplied SCSI initiator identity.
    pub initiator: Option<String>,
}

impl CdbPacket {
    pub fn new(cdb: Vec<u8>, data: Vec<u8>) -> Self {
        Self {
            cdb,
            data,
            initiator: None,
        }
    }

    pub fn with_initiator(cdb: Vec<u8>, data: Vec<u8>, initiator: impl Into<String>) -> Self {
        Self {
            cdb,
            data,
            initiator: Some(initiator.into()),
        }
    }

    /// Encode the packet into the wire format and write it to `w`.
    pub fn encode<W: Write>(&self, w: &mut W) -> io::Result<()> {
        let cdb_len = self.cdb.len();
        if !matches!(cdb_len, 6 | 10 | 12 | 16) {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                format!("unsupported CDB length: {cdb_len}"),
            ));
        }
        let data_len = self.data.len();
        if data_len > MAX_DATA_LEN {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "data too large",
            ));
        }
        let initiator = self
            .initiator
            .as_deref()
            .map(str::trim)
            .filter(|value| !value.is_empty());
        if let Some(initiator) = initiator {
            if initiator.len() > MAX_INITIATOR_LEN {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidInput,
                    "initiator too long",
                ));
            }
            w.write_all(&[EXTENDED_REQUEST_MARKER])?;
            w.write_all(&[EXTENDED_REQUEST_VERSION])?;
            w.write_all(&[initiator.len() as u8])?;
            w.write_all(initiator.as_bytes())?;
            w.write_all(&[cdb_len as u8])?;
            w.write_all(&self.cdb)?;
            w.write_all(&(data_len as u32).to_be_bytes())?;
            w.write_all(&self.data)?;
            return Ok(());
        }
        w.write_all(&[cdb_len as u8])?;
        w.write_all(&self.cdb)?;
        w.write_all(&(data_len as u32).to_be_bytes())?;
        w.write_all(&self.data)?;
        Ok(())
    }

    /// Decode only the CDB and data length. Callers that need to avoid a fresh
    /// allocation per hot-path CDB can then read the body into a reusable buffer.
    pub fn decode_header<R: Read>(r: &mut R) -> io::Result<CdbPacketHeader> {
        let mut cdb_len_buf = [0u8; 1];
        r.read_exact(&mut cdb_len_buf)?;
        let mut initiator = None;
        if cdb_len_buf[0] == EXTENDED_REQUEST_MARKER {
            let mut version_buf = [0u8; 1];
            r.read_exact(&mut version_buf)?;
            if version_buf[0] != EXTENDED_REQUEST_VERSION {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidData,
                    format!("unsupported CDB frame version: {}", version_buf[0]),
                ));
            }
            let mut initiator_len_buf = [0u8; 1];
            r.read_exact(&mut initiator_len_buf)?;
            let initiator_len = initiator_len_buf[0] as usize;
            if initiator_len > 0 {
                let mut initiator_buf = vec![0u8; initiator_len];
                r.read_exact(&mut initiator_buf)?;
                let value = String::from_utf8(initiator_buf).map_err(|_| {
                    io::Error::new(io::ErrorKind::InvalidData, "initiator is not utf-8")
                })?;
                let trimmed = value.trim();
                if !trimmed.is_empty() {
                    initiator = Some(trimmed.to_string());
                }
            }
            r.read_exact(&mut cdb_len_buf)?;
        }
        let cdb_len = cdb_len_buf[0] as usize;
        if !matches!(cdb_len, 6 | 10 | 12 | 16) {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!("unsupported CDB length: {cdb_len}"),
            ));
        }

        let mut cdb = vec![0u8; cdb_len];
        r.read_exact(&mut cdb)?;

        let mut data_len_buf = [0u8; 4];
        r.read_exact(&mut data_len_buf)?;
        let data_len = u32::from_be_bytes(data_len_buf) as usize;
        if data_len > MAX_DATA_LEN {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!("data_len {data_len} exceeds maximum {MAX_DATA_LEN}"),
            ));
        }

        Ok(CdbPacketHeader {
            cdb,
            data_len,
            initiator,
        })
    }

    /// Decode a CdbPacket from `r`.
    pub fn decode<R: Read>(r: &mut R) -> io::Result<Self> {
        let header = Self::decode_header(r)?;

        let mut data = vec![0u8; header.data_len];
        r.read_exact(&mut data)?;

        Ok(Self {
            cdb: header.cdb,
            data,
            initiator: header.initiator,
        })
    }
}

// ---------------------------------------------------------------------------
// CdbResponse — response frame
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CdbResponse {
    /// Sense bytes; empty when status == GOOD.
    pub sense: Vec<u8>,
    /// Data-in payload (READ commands etc.; empty for write/control commands).
    pub reply: Vec<u8>,
    /// SCSI status byte: 0x00 GOOD, 0x02 CHECK CONDITION, 0x08 BUSY.
    pub status: u8,
}

impl CdbResponse {
    /// Build a GOOD response with optional data-in payload.
    pub fn good(reply: Vec<u8>) -> Self {
        Self {
            sense: vec![],
            reply,
            status: SCSI_STATUS_GOOD,
        }
    }

    /// Build a CHECK CONDITION response with fixed-format sense bytes.
    pub fn check_condition(sense: Vec<u8>) -> Self {
        Self {
            sense,
            reply: vec![],
            status: SCSI_STATUS_CHECK_CONDITION,
        }
    }

    /// Build a BUSY response (data-plane temporarily unavailable).
    pub fn busy() -> Self {
        Self {
            sense: vec![],
            reply: vec![],
            status: SCSI_STATUS_BUSY,
        }
    }

    /// Encode the response into the wire format and write it to `w`.
    pub fn encode<W: Write>(&self, w: &mut W) -> io::Result<()> {
        let sense_len = self.sense.len();
        if sense_len > MAX_SENSE_LEN {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "sense too long",
            ));
        }
        let reply_len = self.reply.len();
        if reply_len > MAX_DATA_LEN {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "reply too large",
            ));
        }
        w.write_all(&[sense_len as u8])?;
        w.write_all(&self.sense)?;
        w.write_all(&(reply_len as u32).to_be_bytes())?;
        w.write_all(&self.reply)?;
        w.write_all(&[self.status])?;
        Ok(())
    }

    /// Decode a CdbResponse from `r`.
    pub fn decode<R: Read>(r: &mut R) -> io::Result<Self> {
        let mut sense_len_buf = [0u8; 1];
        r.read_exact(&mut sense_len_buf)?;
        let sense_len = sense_len_buf[0] as usize;

        let mut sense = vec![0u8; sense_len];
        r.read_exact(&mut sense)?;

        let mut reply_len_buf = [0u8; 4];
        r.read_exact(&mut reply_len_buf)?;
        let reply_len = u32::from_be_bytes(reply_len_buf) as usize;
        if reply_len > MAX_DATA_LEN {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                format!("reply_len {reply_len} exceeds maximum {MAX_DATA_LEN}"),
            ));
        }

        let mut reply = vec![0u8; reply_len];
        r.read_exact(&mut reply)?;

        let mut status_buf = [0u8; 1];
        r.read_exact(&mut status_buf)?;

        Ok(Self {
            sense,
            reply,
            status: status_buf[0],
        })
    }
}
