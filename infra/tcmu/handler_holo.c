#define _GNU_SOURCE

#include <arpa/inet.h>
#include <errno.h>
#include <inttypes.h>
#include <pthread.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <signal.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/time.h>
#include <sys/uio.h>
#include <sys/un.h>
#include <sys/types.h>
#include <time.h>
#include <unistd.h>

#include <scsi/scsi.h>

#include "libtcmu_common.h"
#include "tcmu-runner.h"
#include "tcmur_device.h"

#define HOLO_MAX_REPLY (16U * 1024U * 1024U)
#define HOLO_MAX_CONTROL_DATA_OUT (1U * 1024U * 1024U)
#define HOLO_MAX_WRITE_ATTRIBUTE_DATA_OUT (64U * 1024U)
#define HOLO_IOV_BATCH_MAX 1024
#define HOLO_EXTENDED_REQUEST_MARKER 0xFF
#define HOLO_EXTENDED_REQUEST_VERSION 0x01
#define HOLO_MAX_INITIATOR_LEN 255
#define HOLO_WATCHDOG_THRESHOLDS 4

enum holo_timing_bucket {
	HOLO_TIMING_READ = 0,
	HOLO_TIMING_WRITE = 1,
	HOLO_TIMING_OTHER = 2,
	HOLO_TIMING_BUCKETS = 3,
};

struct holo_timing_acc {
	uint64_t cmds;
	uint64_t data_out_bytes;
	uint64_t data_in_bytes;
	uint64_t total_us;
	uint64_t connect_us;
	uint64_t request_us;
	uint64_t response_us;
	uint64_t max_total_us;
};

struct holo_timing_state {
	uint64_t every;
	uint64_t since_log;
	struct holo_timing_acc buckets[HOLO_TIMING_BUCKETS];
};

struct holo_state {
	int fd;
	char socket_path[sizeof(((struct sockaddr_un *)0)->sun_path)];
	struct holo_timing_state timing;
	pthread_mutex_t watchdog_lock;
	pthread_t watchdog_thread;
	bool watchdog_running;
	bool watchdog_stop;
	bool active;
	uint64_t active_seq;
	uint64_t active_start_us;
	uint32_t active_data_out_len;
	uint8_t active_opcode;
	uint8_t active_cdb[16];
	uint8_t active_cdb_len;
	size_t active_threshold_idx;
	char active_phase[32];
};

static const uint64_t watchdog_threshold_us[HOLO_WATCHDOG_THRESHOLDS] = {
	1000000ULL,
	5000000ULL,
	15000000ULL,
	25000000ULL,
};

static uint64_t monotonic_us(void)
{
	struct timespec ts;

	if (clock_gettime(CLOCK_MONOTONIC, &ts) < 0) {
		return 0;
	}
	return ((uint64_t)ts.tv_sec * 1000000ULL) + ((uint64_t)ts.tv_nsec / 1000ULL);
}

static uint64_t timing_every_commands(void)
{
	const char *raw = getenv("HOLO_TCMU_TIMING_EVERY");
	char *end = NULL;
	unsigned long long every;

	if (!raw || raw[0] == '\0') {
		return 0;
	}

	every = strtoull(raw, &end, 10);
	if (!end || *end != '\0' || every == 0 || every > 1000000ULL) {
		return 0;
	}
	return (uint64_t)every;
}

static void format_cdb_hex(const uint8_t *cdb, uint8_t cdb_len, char *out, size_t out_len)
{
	size_t pos = 0;
	uint8_t i;

	if (!out || out_len == 0) {
		return;
	}
	out[0] = '\0';
	for (i = 0; i < cdb_len && i < 16; i++) {
		int n = snprintf(out + pos, out_len - pos, "%s%02X", i == 0 ? "" : " ", cdb[i]);
		if (n < 0 || (size_t)n >= out_len - pos) {
			out[out_len - 1] = '\0';
			return;
		}
		pos += (size_t)n;
	}
}

static void holo_watchdog_set_phase(struct holo_state *state, const char *phase)
{
	if (!state || !phase) {
		return;
	}
	pthread_mutex_lock(&state->watchdog_lock);
	if (state->active) {
		snprintf(state->active_phase, sizeof(state->active_phase), "%s", phase);
	}
	pthread_mutex_unlock(&state->watchdog_lock);
}

static void holo_watchdog_begin(
	struct holo_state *state,
	uint8_t opcode,
	const uint8_t *cdb,
	uint8_t cdb_len,
	uint32_t data_out_len)
{
	if (!state) {
		return;
	}
	pthread_mutex_lock(&state->watchdog_lock);
	state->active = true;
	state->active_seq++;
	state->active_start_us = monotonic_us();
	state->active_opcode = opcode;
	state->active_cdb_len = cdb_len > sizeof(state->active_cdb) ? sizeof(state->active_cdb) : cdb_len;
	if (cdb && state->active_cdb_len > 0) {
		memcpy(state->active_cdb, cdb, state->active_cdb_len);
	}
	state->active_data_out_len = data_out_len;
	state->active_threshold_idx = 0;
	snprintf(state->active_phase, sizeof(state->active_phase), "%s", "connect");
	pthread_mutex_unlock(&state->watchdog_lock);
}

static void holo_watchdog_finish(struct holo_state *state)
{
	if (!state) {
		return;
	}
	pthread_mutex_lock(&state->watchdog_lock);
	state->active = false;
	pthread_mutex_unlock(&state->watchdog_lock);
}

static void *holo_watchdog_loop(void *arg)
{
	struct holo_state *state = arg;

	for (;;) {
		uint64_t now;
		bool should_log = false;
		uint64_t elapsed_us = 0;
		uint64_t seq = 0;
		uint8_t opcode = 0;
		uint32_t data_out_len = 0;
		char phase[sizeof(state->active_phase)];
		char cdb_hex[64];

		usleep(250000);
		pthread_mutex_lock(&state->watchdog_lock);
		if (state->watchdog_stop) {
			pthread_mutex_unlock(&state->watchdog_lock);
			return NULL;
		}
		if (state->active && state->active_threshold_idx < HOLO_WATCHDOG_THRESHOLDS) {
			now = monotonic_us();
			elapsed_us = now - state->active_start_us;
			if (elapsed_us >= watchdog_threshold_us[state->active_threshold_idx]) {
				should_log = true;
				seq = state->active_seq;
				opcode = state->active_opcode;
				data_out_len = state->active_data_out_len;
				snprintf(phase, sizeof(phase), "%s", state->active_phase);
				format_cdb_hex(state->active_cdb, state->active_cdb_len, cdb_hex, sizeof(cdb_hex));
				state->active_threshold_idx++;
			}
		}
		pthread_mutex_unlock(&state->watchdog_lock);

		if (should_log) {
			tcmu_warn("holo slow_cdb_inflight socket=%s seq=%" PRIu64
				  " opcode=0x%02X phase=%s elapsed_us=%" PRIu64
				  " data_out=%" PRIu32 " cdb=[%s]\n",
				  state->socket_path[0] ? state->socket_path : "(unknown)",
				  seq, opcode, phase, elapsed_us, data_out_len, cdb_hex);
		}
	}
}

static enum holo_timing_bucket timing_bucket_for_opcode(uint8_t opcode)
{
	switch (opcode) {
	case READ_6:
	case 0x88: /* READ(16) */
		return HOLO_TIMING_READ;
	case WRITE_6:
	case WRITE_10:
	case WRITE_12:
	case 0x8A: /* WRITE(16) */
		return HOLO_TIMING_WRITE;
	default:
		return HOLO_TIMING_OTHER;
	}
}

static const char *timing_bucket_name(enum holo_timing_bucket bucket)
{
	switch (bucket) {
	case HOLO_TIMING_READ:
		return "read";
	case HOLO_TIMING_WRITE:
		return "write";
	case HOLO_TIMING_OTHER:
	default:
		return "other";
	}
}

static uint64_t avg_us(uint64_t total_us, uint64_t count)
{
	if (count == 0) {
		return 0;
	}
	return total_us / count;
}

static void holo_timing_flush(struct holo_timing_state *timing, const char *socket_path)
{
	size_t i;

	if (!timing || timing->every == 0) {
		return;
	}

	for (i = 0; i < HOLO_TIMING_BUCKETS; i++) {
		struct holo_timing_acc *acc = &timing->buckets[i];
		if (acc->cmds == 0) {
			continue;
		}
		tcmu_info("holo timing socket=%s bucket=%s cmds=%" PRIu64
			  " data_out=%" PRIu64 " data_in=%" PRIu64
			  " avg_total_us=%" PRIu64 " avg_connect_us=%" PRIu64
			  " avg_request_us=%" PRIu64 " avg_response_us=%" PRIu64
			  " max_total_us=%" PRIu64 "\n",
			  socket_path ? socket_path : "(unknown)", timing_bucket_name((enum holo_timing_bucket)i),
			  acc->cmds, acc->data_out_bytes, acc->data_in_bytes,
			  avg_us(acc->total_us, acc->cmds), avg_us(acc->connect_us, acc->cmds),
			  avg_us(acc->request_us, acc->cmds), avg_us(acc->response_us, acc->cmds),
			  acc->max_total_us);
	}

	memset(timing->buckets, 0, sizeof(timing->buckets));
	timing->since_log = 0;
}

static void holo_timing_record(
	struct holo_state *state,
	uint8_t opcode,
	uint32_t data_out_len,
	uint32_t data_in_len,
	uint64_t total_us,
	uint64_t connect_us,
	uint64_t request_us,
	uint64_t response_us)
{
	enum holo_timing_bucket bucket;
	struct holo_timing_acc *acc;

	if (!state || state->timing.every == 0) {
		return;
	}

	bucket = timing_bucket_for_opcode(opcode);
	acc = &state->timing.buckets[bucket];
	acc->cmds++;
	acc->data_out_bytes += data_out_len;
	acc->data_in_bytes += data_in_len;
	acc->total_us += total_us;
	acc->connect_us += connect_us;
	acc->request_us += request_us;
	acc->response_us += response_us;
	if (total_us > acc->max_total_us) {
		acc->max_total_us = total_us;
	}

	state->timing.since_log++;
	if (state->timing.since_log >= state->timing.every) {
		holo_timing_flush(&state->timing, state->socket_path);
	}
}

static int holo_validate_peer_credentials(int fd, const char *socket_path)
{
#ifdef SO_PEERCRED
	struct ucred cred;
	socklen_t len = sizeof(cred);
	struct stat st;

	if (getsockopt(fd, SOL_SOCKET, SO_PEERCRED, &cred, &len) < 0) {
		return -errno;
	}
	if (!socket_path || socket_path[0] == '\0') {
		return -EINVAL;
	}
	if (stat(socket_path, &st) < 0) {
		return -errno;
	}
	/*
	 * Allow only root or the owner of the bound socket file.
	 * This preserves least-privilege deployments where the handler runs
	 * as a non-root service user and tcmu-runner connects as root.
	 */
	if (cred.uid != 0 && cred.uid != st.st_uid) {
		return -EACCES;
	}
#else
	(void)fd;
	(void)socket_path;
#endif
	return 0;
}

static bool cdb_has_data_out(uint8_t opcode)
{
	switch (opcode) {
	case WRITE_6:
	case WRITE_10:
	case WRITE_12:
	case 0x8A: /* WRITE(16) */
	case MODE_SELECT:
	case MODE_SELECT_10:
	case 0x5F: /* PERSISTENT RESERVE OUT */
	case 0x8D: /* WRITE ATTRIBUTE */
		return true;
	default:
		return false;
	}
}

static bool cdb_is_write_opcode(uint8_t opcode)
{
	switch (opcode) {
	case WRITE_6:
	case WRITE_10:
	case WRITE_12:
	case 0x8A: /* WRITE(16) */
		return true;
	default:
		return false;
	}
}

static size_t cdb_max_data_out_len(uint8_t opcode)
{
	switch (opcode) {
	case WRITE_6:
	case WRITE_10:
	case WRITE_12:
	case 0x8A: /* WRITE(16) */
		return UINT32_MAX;
	case 0x8D: /* WRITE ATTRIBUTE */
		return HOLO_MAX_WRITE_ATTRIBUTE_DATA_OUT;
	case MODE_SELECT:
	case MODE_SELECT_10:
	case 0x5F: /* PERSISTENT RESERVE OUT */
		return HOLO_MAX_CONTROL_DATA_OUT;
	default:
		return HOLO_MAX_CONTROL_DATA_OUT;
	}
}

static const char *holo_initiator_id(void)
{
	const char *raw = getenv("HOLO_TCMU_INITIATOR_ID");
	if (!raw || raw[0] == '\0') {
		raw = getenv("HOLO_SCSI_INITIATOR");
	}
	if (!raw || raw[0] == '\0') {
		return NULL;
	}
	return raw;
}

static size_t holo_initiator_len(const char *initiator)
{
	size_t len;

	if (!initiator) {
		return 0;
	}
	len = strnlen(initiator, HOLO_MAX_INITIATOR_LEN + 1);
	if (len == 0 || len > HOLO_MAX_INITIATOR_LEN) {
		return 0;
	}
	return len;
}

static size_t iovec_total_len(const struct iovec *iov, size_t iov_cnt)
{
	size_t total = 0;
	size_t i;

	if (!iov || iov_cnt == 0) {
		return 0;
	}

	for (i = 0; i < iov_cnt; i++) {
		if (iov[i].iov_len > SIZE_MAX - total) {
			return SIZE_MAX;
		}
		total += iov[i].iov_len;
	}
	return total;
}

static size_t cdb_data_out_len(
	const uint8_t *cdb,
	int cdb_len,
	size_t fallback,
	size_t available)
{
	size_t want = fallback;

	if (!cdb || cdb_len <= 0) {
		return want;
	}

	/*
	 * WRITE CDB transfer length is block-count in fixed-block mode, not bytes.
	 * The transport iovec already carries the actual byte payload.
	 */
	if (cdb_is_write_opcode(cdb[0])) {
		if (available > 0) {
			return available;
		}
		return want;
	}

	switch (cdb[0]) {
	case MODE_SELECT:
		if (cdb_len >= 5) {
			want = (size_t)cdb[4];
		}
		break;
	case MODE_SELECT_10:
		if (cdb_len >= 9) {
			want = ((size_t)cdb[7] << 8) | (size_t)cdb[8];
		}
		break;
	case 0x5F: /* PERSISTENT RESERVE OUT */
		if (cdb_len >= 9) {
			want = ((size_t)cdb[7] << 8) | (size_t)cdb[8];
		}
		break;
	case 0x8D: /* WRITE ATTRIBUTE */
		if (cdb_len >= 14) {
			want = ((size_t)cdb[10] << 24) | ((size_t)cdb[11] << 16) |
			       ((size_t)cdb[12] << 8) | (size_t)cdb[13];
		}
		break;
	default:
		break;
	}

	if (available > 0 && want > available) {
		want = available;
	}
	return want;
}

static int socket_timeout_sec(void)
{
	const char *raw = getenv("HOLO_TCMU_SOCKET_TIMEOUT_SEC");
	char *end = NULL;
	long sec;

	if (!raw || raw[0] == '\0') {
		return 30;
	}

	sec = strtol(raw, &end, 10);
	if (!end || *end != '\0' || sec <= 0 || sec > 300) {
		return 30;
	}
	return (int)sec;
}

static int socket_buffer_bytes(void)
{
	const char *raw = getenv("HOLO_TCMU_SOCKET_BUF_BYTES");
	char *end = NULL;
	long bytes;

	if (!raw || raw[0] == '\0') {
		return 0;
	}

	bytes = strtol(raw, &end, 10);
	if (!end || *end != '\0' || bytes < 65536 || bytes > (64 * 1024 * 1024)) {
		return 0;
	}
	return (int)bytes;
}

static void configure_socket_buffers(int fd)
{
	int bytes = socket_buffer_bytes();

	if (bytes <= 0) {
		return;
	}

	if (setsockopt(fd, SOL_SOCKET, SO_SNDBUF, &bytes, sizeof(bytes)) < 0) {
		tcmu_warn("holo: failed to set SO_SNDBUF=%d: %s\n",
			  bytes, strerror(errno));
	}
	if (setsockopt(fd, SOL_SOCKET, SO_RCVBUF, &bytes, sizeof(bytes)) < 0) {
		tcmu_warn("holo: failed to set SO_RCVBUF=%d: %s\n",
			  bytes, strerror(errno));
	}
}

static int write_full(int fd, const void *buf, size_t len)
{
	const uint8_t *p = buf;
	size_t left = len;
	while (left > 0) {
		ssize_t n = write(fd, p, left);
		if (n < 0) {
			if (errno == EINTR) {
				continue;
			}
			if (errno == EAGAIN || errno == EWOULDBLOCK) {
				return -ETIMEDOUT;
			}
			return -errno;
		}
		if (n == 0) {
			return -EPIPE;
		}
		p += (size_t)n;
		left -= (size_t)n;
	}
	return 0;
}

static int read_full(int fd, void *buf, size_t len)
{
	uint8_t *p = buf;
	size_t left = len;
	while (left > 0) {
		ssize_t n = read(fd, p, left);
		if (n < 0) {
			if (errno == EINTR) {
				continue;
			}
			if (errno == EAGAIN || errno == EWOULDBLOCK) {
				return -ETIMEDOUT;
			}
			return -errno;
		}
		if (n == 0) {
			return -EPIPE;
		}
		p += (size_t)n;
		left -= (size_t)n;
	}
	return 0;
}

static int write_iovec_limited_full(int fd, const struct iovec *iov, size_t iov_cnt, size_t len)
{
	size_t left = len;
	size_t idx;
	size_t offset = 0;

	if (left == 0) {
		return 0;
	}
	if (!iov || iov_cnt == 0) {
		return -EINVAL;
	}

	idx = 0;
	while (left > 0) {
		struct iovec batch[HOLO_IOV_BATCH_MAX];
		size_t batch_count = 0;
		size_t batch_len = 0;
		size_t scan = idx;
		size_t scan_offset = offset;
		ssize_t n;
		size_t advanced;

		while (scan < iov_cnt && batch_count < HOLO_IOV_BATCH_MAX && batch_len < left) {
			size_t available = iov[scan].iov_len;
			size_t take;

			if (scan_offset >= available) {
				scan++;
				scan_offset = 0;
				continue;
			}
			available -= scan_offset;
			take = available;
			if (take > left - batch_len) {
				take = left - batch_len;
			}
			batch[batch_count].iov_base = (uint8_t *)iov[scan].iov_base + scan_offset;
			batch[batch_count].iov_len = take;
			batch_count++;
			batch_len += take;
			scan++;
			scan_offset = 0;
		}

		if (batch_count == 0) {
			return -EINVAL;
		}

		n = writev(fd, batch, (int)batch_count);
		if (n < 0) {
			if (errno == EINTR) {
				continue;
			}
			if (errno == EAGAIN || errno == EWOULDBLOCK) {
				return -ETIMEDOUT;
			}
			return -errno;
		}
		if (n == 0) {
			return -EPIPE;
		}

		advanced = (size_t)n;
		left -= advanced;
		while (advanced > 0 && idx < iov_cnt) {
			size_t available = iov[idx].iov_len - offset;
			if (available > advanced) {
				offset += advanced;
				advanced = 0;
			} else {
				advanced -= available;
				idx++;
				offset = 0;
			}
		}
	}

	return 0;
}

static int read_iovec_limited_full(int fd, const struct iovec *iov, size_t iov_cnt, size_t len)
{
	size_t left = len;
	size_t idx;
	size_t offset = 0;

	if (left == 0) {
		return 0;
	}
	if (!iov || iov_cnt == 0) {
		return -EINVAL;
	}

	idx = 0;
	while (left > 0) {
		struct iovec batch[HOLO_IOV_BATCH_MAX];
		size_t batch_count = 0;
		size_t batch_len = 0;
		size_t scan = idx;
		size_t scan_offset = offset;
		ssize_t n;
		size_t advanced;

		while (scan < iov_cnt && batch_count < HOLO_IOV_BATCH_MAX && batch_len < left) {
			size_t available = iov[scan].iov_len;
			size_t take;

			if (scan_offset >= available) {
				scan++;
				scan_offset = 0;
				continue;
			}
			available -= scan_offset;
			take = available;
			if (take > left - batch_len) {
				take = left - batch_len;
			}
			batch[batch_count].iov_base = (uint8_t *)iov[scan].iov_base + scan_offset;
			batch[batch_count].iov_len = take;
			batch_count++;
			batch_len += take;
			scan++;
			scan_offset = 0;
		}

		if (batch_count == 0) {
			return -EINVAL;
		}

		n = readv(fd, batch, (int)batch_count);
		if (n < 0) {
			if (errno == EINTR) {
				continue;
			}
			if (errno == EAGAIN || errno == EWOULDBLOCK) {
				return -ETIMEDOUT;
			}
			return -errno;
		}
		if (n == 0) {
			return -EPIPE;
		}

		advanced = (size_t)n;
		left -= advanced;
		while (advanced > 0 && idx < iov_cnt) {
			size_t available = iov[idx].iov_len - offset;
			if (available > advanced) {
				offset += advanced;
				advanced = 0;
			} else {
				advanced -= available;
				idx++;
				offset = 0;
			}
		}
	}

	return 0;
}

static int discard_full(int fd, size_t len)
{
	uint8_t discard[8192];
	size_t left = len;

	while (left > 0) {
		size_t chunk = left > sizeof(discard) ? sizeof(discard) : left;
		int ret = read_full(fd, discard, chunk);
		if (ret < 0) {
			return ret;
		}
		left -= chunk;
	}
	return 0;
}

static void holo_disconnect(struct holo_state *state)
{
	if (!state || state->fd < 0) {
		return;
	}
	close(state->fd);
	state->fd = -1;
}

static int holo_connect(struct holo_state *state)
{
	struct sockaddr_un addr;
	int fd;
	int ret;
	struct timeval tv = {0};

	if (!state) {
		return -EINVAL;
	}
	if (state->fd >= 0) {
		return 0;
	}

	fd = socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
	if (fd < 0) {
		return -errno;
	}

	memset(&addr, 0, sizeof(addr));
	addr.sun_family = AF_UNIX;
	if (snprintf(addr.sun_path, sizeof(addr.sun_path), "%s", state->socket_path) >=
	    (int)sizeof(addr.sun_path)) {
		close(fd);
		return -ENAMETOOLONG;
	}

	if (connect(fd, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
		int saved = errno;
		close(fd);
		return -saved;
	}

	tv.tv_sec = socket_timeout_sec();
	tv.tv_usec = 0;
	if (setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv)) < 0 ||
	    setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof(tv)) < 0) {
		int saved = errno;
		close(fd);
		return -saved;
	}
	configure_socket_buffers(fd);
	ret = holo_validate_peer_credentials(fd, state->socket_path);
	if (ret < 0) {
		close(fd);
		return ret;
	}

	state->fd = fd;
	return 0;
}

static const char *cfg_socket_path(const char *cfgstring)
{
	const char *slash;
	if (!cfgstring) {
		return NULL;
	}
	slash = strchr(cfgstring, '/');
	if (!slash) {
		return NULL;
	}
	return slash + 1;
}

static bool holo_check_config(const char *cfgstring, char **reason)
{
	const char *sock = cfg_socket_path(cfgstring);
	if (sock == NULL || sock[0] == '\0') {
		if (reason) {
			int n = asprintf(reason,
				       "cfgstring must include socket path after subtype, got '%s'",
				       cfgstring ? cfgstring : "(null)");
			if (n < 0) {
				*reason = NULL;
			}
		}
		return false;
	}
	return true;
}

static int holo_open(struct tcmu_device *dev, bool reopen)
{
	struct holo_state *state;
	const char *sock;
	int ret;

	(void)reopen;

	state = calloc(1, sizeof(*state));
	if (!state) {
		return -ENOMEM;
	}
	state->fd = -1;
	state->timing.every = timing_every_commands();
	if (pthread_mutex_init(&state->watchdog_lock, NULL) != 0) {
		free(state);
		return -ENOMEM;
	}

	sock = cfg_socket_path(tcmu_dev_get_cfgstring(dev));
	if (!sock || sock[0] == '\0') {
		tcmu_err("holo: invalid cfgstring '%s'\n", tcmu_dev_get_cfgstring(dev));
		pthread_mutex_destroy(&state->watchdog_lock);
		free(state);
		return -EINVAL;
	}
	if (snprintf(state->socket_path, sizeof(state->socket_path), "%s", sock) >=
	    (int)sizeof(state->socket_path)) {
		tcmu_err("holo: socket path too long: %s\n", sock);
		pthread_mutex_destroy(&state->watchdog_lock);
		free(state);
		return -ENAMETOOLONG;
	}
	if (pthread_create(&state->watchdog_thread, NULL, holo_watchdog_loop, state) == 0) {
		state->watchdog_running = true;
	} else {
		tcmu_warn("holo: slow CDB watchdog disabled for %s\n", state->socket_path);
	}

	/*
	 * Connection is best-effort at open time. If control-plane publishes
	 * target before socket becomes ready, handle_cmd() will retry connect.
	 */
	ret = holo_connect(state);
	if (ret < 0) {
	tcmu_warn("holo: defer socket connect (%s): %s\n",
		  state->socket_path, strerror(-ret));
	}

	tcmur_dev_set_private(dev, state);
	return 0;
}

static void holo_close(struct tcmu_device *dev)
{
	struct holo_state *state = tcmur_dev_get_private(dev);
	if (!state) {
		return;
	}
	holo_timing_flush(&state->timing, state->socket_path);
	pthread_mutex_lock(&state->watchdog_lock);
	state->watchdog_stop = true;
	pthread_mutex_unlock(&state->watchdog_lock);
	if (state->watchdog_running) {
		pthread_join(state->watchdog_thread, NULL);
	}
	holo_disconnect(state);
	pthread_mutex_destroy(&state->watchdog_lock);
	free(state);
	tcmur_dev_set_private(dev, NULL);
}

static int holo_handle_cmd(struct tcmu_device *dev, struct tcmur_cmd *runner_cmd)
{
	struct tcmulib_cmd *cmd = runner_cmd->lib_cmd;
	struct holo_state *state = tcmur_dev_get_private(dev);
	uint8_t *cdb;
	int cdb_len;
	uint8_t cdb_len_u8;
	uint32_t data_out_len = 0;
	uint8_t *data_out = NULL;
	bool direct_data_out = false;
	int ret;
	uint8_t sense_len = 0;
	uint32_t reply_len_be = 0;
	uint32_t reply_len = 0;
	uint8_t status = 0;
	uint8_t opcode = 0;
	uint64_t timing_start = 0;
	uint64_t timing_connect_start = 0;
	uint64_t timing_request_start = 0;
	uint64_t timing_response_start = 0;
	uint64_t timing_connect_us = 0;
	uint64_t timing_request_us = 0;
	uint64_t timing_response_us = 0;
	const char *initiator = NULL;
	size_t initiator_len = 0;
	uint8_t extended_marker = HOLO_EXTENDED_REQUEST_MARKER;
	uint8_t extended_version = HOLO_EXTENDED_REQUEST_VERSION;
	uint8_t initiator_len_u8 = 0;

	if (!cmd || !state) {
		return TCMU_STS_HW_ERR;
	}

	cdb = cmd->cdb;
	cdb_len = tcmu_cdb_get_length(cdb);
	if (cdb_len <= 0 || cdb_len > 255) {
		return TCMU_STS_INVALID_CDB;
	}
	if (!(cdb_len == 6 || cdb_len == 10 || cdb_len == 12 || cdb_len == 16)) {
		return TCMU_STS_INVALID_CDB;
	}
	cdb_len_u8 = (uint8_t)cdb_len;
	opcode = cdb[0];
	initiator = holo_initiator_id();
	initiator_len = holo_initiator_len(initiator);
	initiator_len_u8 = (uint8_t)initiator_len;
	if (state->timing.every > 0) {
		timing_start = monotonic_us();
	}

	if (cdb_has_data_out(opcode)) {
		size_t available = iovec_total_len(cmd->iovec, cmd->iov_cnt);
		size_t want = cdb_data_out_len(cdb, cdb_len, runner_cmd->requested, available);
		size_t max_data_out = cdb_max_data_out_len(opcode);
		if (want > max_data_out) {
			return TCMU_STS_INVALID_CDB;
		}
		if (want > UINT32_MAX) {
			return TCMU_STS_INVALID_CDB;
		}
		data_out_len = (uint32_t)want;
		direct_data_out = cdb_is_write_opcode(opcode) && data_out_len > 0;
		if (data_out_len > 0 && !direct_data_out) {
			data_out = malloc(data_out_len);
			if (!data_out) {
				return TCMU_STS_NO_RESOURCE;
			}
			tcmu_memcpy_from_iovec(data_out, data_out_len, cmd->iovec, cmd->iov_cnt);
		}
	}
	holo_watchdog_begin(state, opcode, cdb, cdb_len_u8, data_out_len);

	if (state->timing.every > 0) {
		timing_connect_start = monotonic_us();
	}
	ret = holo_connect(state);
	if (state->timing.every > 0) {
		timing_connect_us = monotonic_us() - timing_connect_start;
	}
	if (ret < 0) {
		free(data_out);
		holo_watchdog_finish(state);
		return TCMU_STS_BUSY;
	}

	holo_watchdog_set_phase(state, "send_request");
	if (state->timing.every > 0) {
		timing_request_start = monotonic_us();
	}
	if (initiator_len > 0) {
		ret = write_full(state->fd, &extended_marker, sizeof(extended_marker));
		if (ret == 0) {
			ret = write_full(state->fd, &extended_version, sizeof(extended_version));
		}
		if (ret == 0) {
			ret = write_full(state->fd, &initiator_len_u8, sizeof(initiator_len_u8));
		}
		if (ret == 0) {
			ret = write_full(state->fd, initiator, initiator_len);
		}
	} else {
		ret = write_full(state->fd, &cdb_len_u8, sizeof(cdb_len_u8));
	}
	if (ret == 0 && initiator_len > 0) {
		ret = write_full(state->fd, &cdb_len_u8, sizeof(cdb_len_u8));
	}
	if (ret == 0) {
		ret = write_full(state->fd, cdb, (size_t)cdb_len);
	}
	if (ret == 0) {
		data_out_len = htonl(data_out_len);
		ret = write_full(state->fd, &data_out_len, sizeof(data_out_len));
		data_out_len = ntohl(data_out_len);
	}
	if (ret == 0 && data_out_len > 0) {
		if (direct_data_out) {
			ret = write_iovec_limited_full(state->fd, cmd->iovec, cmd->iov_cnt,
						      data_out_len);
		} else {
			ret = write_full(state->fd, data_out, data_out_len);
		}
	}
	if (state->timing.every > 0) {
		timing_request_us = monotonic_us() - timing_request_start;
	}
	free(data_out);
	if (ret < 0) {
		holo_disconnect(state);
		holo_watchdog_finish(state);
		return TCMU_STS_BUSY;
	}

	holo_watchdog_set_phase(state, "read_response");
	if (state->timing.every > 0) {
		timing_response_start = monotonic_us();
	}
	ret = read_full(state->fd, &sense_len, sizeof(sense_len));
	if (ret < 0) {
		holo_disconnect(state);
		holo_watchdog_finish(state);
		return TCMU_STS_BUSY;
	}
	if (sense_len > 0) {
		size_t copy_len = sense_len;
		if (copy_len > SENSE_BUFFERSIZE) {
			copy_len = SENSE_BUFFERSIZE;
		}
		ret = read_full(state->fd, cmd->sense_buf, copy_len);
		if (ret < 0) {
			holo_disconnect(state);
			holo_watchdog_finish(state);
			return TCMU_STS_BUSY;
		}
		if (sense_len > copy_len) {
			uint8_t discard[64];
			size_t left = (size_t)sense_len - copy_len;
			while (left > 0) {
				size_t chunk = left > sizeof(discard) ? sizeof(discard) : left;
				ret = read_full(state->fd, discard, chunk);
				if (ret < 0) {
					holo_disconnect(state);
					holo_watchdog_finish(state);
					return TCMU_STS_BUSY;
				}
				left -= chunk;
			}
		}
	}

	ret = read_full(state->fd, &reply_len_be, sizeof(reply_len_be));
	if (ret < 0) {
		holo_disconnect(state);
		holo_watchdog_finish(state);
		return TCMU_STS_BUSY;
	}
	reply_len = ntohl(reply_len_be);
	if (reply_len > HOLO_MAX_REPLY) {
		holo_disconnect(state);
		holo_watchdog_finish(state);
		return TCMU_STS_RD_ERR;
	}
	if (reply_len > 0) {
		size_t copy_len = reply_len;
		if (cmd->iov_cnt == 0) {
			copy_len = 0;
		} else if (runner_cmd->requested > 0 && copy_len > runner_cmd->requested) {
			copy_len = runner_cmd->requested;
		}
		if (runner_cmd->requested > copy_len && cmd->iov_cnt > 0) {
			tcmu_iovec_zero(cmd->iovec, cmd->iov_cnt);
		}
		if (copy_len > 0) {
			ret = read_iovec_limited_full(state->fd, cmd->iovec, cmd->iov_cnt,
						     copy_len);
			if (ret < 0) {
				holo_disconnect(state);
				holo_watchdog_finish(state);
				return TCMU_STS_BUSY;
			}
		}
		if (reply_len > copy_len) {
			ret = discard_full(state->fd, reply_len - copy_len);
			if (ret < 0) {
				holo_disconnect(state);
				holo_watchdog_finish(state);
				return TCMU_STS_BUSY;
			}
		}
	}

	ret = read_full(state->fd, &status, sizeof(status));
	if (ret < 0) {
		holo_disconnect(state);
		holo_watchdog_finish(state);
		return TCMU_STS_BUSY;
	}
	if (state->timing.every > 0) {
		uint64_t now = monotonic_us();
		timing_response_us = now - timing_response_start;
		holo_timing_record(state, opcode, data_out_len, reply_len,
				    now - timing_start, timing_connect_us, timing_request_us,
				    timing_response_us);
	}

	switch (status) {
	case 0x00: /* GOOD */
		holo_watchdog_finish(state);
		return TCMU_STS_OK;
	case 0x02: /* CHECK CONDITION */
		holo_watchdog_finish(state);
		return TCMU_STS_PASSTHROUGH_ERR;
	case 0x08: /* BUSY */
		holo_watchdog_finish(state);
		return TCMU_STS_BUSY;
	default:
		holo_watchdog_finish(state);
		return TCMU_STS_HW_ERR;
	}
}

static const char holo_cfg_desc[] =
	"Socket path to holo data-plane CDB server (example: /run/holo/cdb-<publication>.sock)";

static struct tcmur_handler holo_handler = {
	.name = "holo TCMU Socket Bridge",
	.subtype = "holo",
	.cfg_desc = holo_cfg_desc,
	.check_config = holo_check_config,
	.open = holo_open,
	.close = holo_close,
	.handle_cmd = holo_handle_cmd,
	/*
	 * Serialize handler execution per device to avoid framing corruption on
	 * a shared UNIX stream socket when multiple IO worker threads are active.
	 */
	.nr_threads = 1,
};

int handler_init(void)
{
	signal(SIGPIPE, SIG_IGN);
	return tcmur_register_handler(&holo_handler);
}
