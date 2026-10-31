#include "../handler_holo.c"
#include <assert.h>

static struct tcmu_mailbox *new_ring(void)
{
	void *map = aligned_alloc(64, 512);
	assert(map);
	memset(map, 0, 512);
	struct tcmu_mailbox *mb = map;
	mb->version = 2;
	mb->flags = 2;
	mb->cmdr_off = 128;
	mb->cmdr_size = 256;
	mb->cmd_head = 112;
	struct tcmu_cmd_entry *ent = (void *)((char *)map + 128);
	ent->hdr.len_op = 112 | TCMU_OP_CMD;
	ent->hdr.cmd_id = 7;
	return mb;
}

int main(void)
{
	struct tcmu_mailbox *mb = new_ring();
	struct tcmu_cmd_entry *ent = (void *)((char *)mb + 128);
	assert(holo_read_len_entry(mb, 512, 7) == ent);
	holo_record_read_len(ent, 8);
	uint32_t actual;
	memcpy(&actual, (char *)&ent->rsp + 4, sizeof(actual));
	assert(actual == 8 && (ent->hdr.uflags & 2));
	assert(mb->cmd_tail == 0 && ent->hdr.cmd_id == 7);
	ent->hdr.uflags = 0;
	holo_record_read_len(ent, 0);
	assert(ent->hdr.uflags == 0);
	assert(holo_read_len_entry(mb, 512, 8) == NULL);
	assert(holo_read_len_entry(NULL, 512, 7) == NULL);
	assert(holo_read_len_entry(mb, sizeof(*mb) - 1, 7) == NULL);
	mb->version = 1;
	assert(holo_read_len_entry(mb, 512, 7) == NULL);
	mb->version = 2;
	mb->flags = 0;
	assert(holo_read_len_entry(mb, 512, 7) == NULL);
	mb->flags = 2;
	mb->cmdr_off = UINT32_MAX;
	assert(holo_read_len_entry(mb, 512, 7) == NULL);
	mb->cmdr_off = 128;
	mb->cmdr_size = 0;
	assert(holo_read_len_entry(mb, 512, 7) == NULL);
	mb->cmdr_size = 1024;
	assert(holo_read_len_entry(mb, 512, 7) == NULL);
	mb->cmdr_size = 256;
	mb->cmd_head = 256;
	assert(holo_read_len_entry(mb, 512, 7) == NULL);
	mb->cmd_head = 112;
	mb->cmd_tail = 1;
	assert(holo_read_len_entry(mb, 512, 7) == NULL);
	mb->cmd_tail = 0;
	ent->hdr.len_op = TCMU_OP_CMD;
	assert(holo_read_len_entry(mb, 512, 7) == NULL);
	ent->hdr.len_op = 8 | TCMU_OP_CMD;
	assert(holo_read_len_entry(mb, 512, 7) == NULL);
	ent->hdr.len_op = 264 | TCMU_OP_CMD;
	assert(holo_read_len_entry(mb, 512, 7) == NULL);
	ent->hdr.len_op = 112 | 3;
	assert(holo_read_len_entry(mb, 512, 7) == NULL);
	ent->hdr.len_op = 112 | TCMU_OP_CMD;
	mb->cmd_tail = 248;
	struct tcmu_cmd_entry_hdr *pad = (void *)((char *)mb + 128 + 248);
	pad->len_op = 8 | TCMU_OP_PAD;
	assert(holo_read_len_entry(mb, 512, 7) == ent);
	assert(mb->cmd_tail == 248);
	pad->len_op = 16 | TCMU_OP_PAD;
	assert(holo_read_len_entry(mb, 512, 7) == NULL);
	uint8_t read6[6] = {0x08, 1, 0, 0, 3, 0};
	assert(holo_fixed_multi_read(read6));
	read6[4] = 1;
	assert(!holo_fixed_multi_read(read6));
	read6[4] = 0;
	assert(holo_fixed_multi_read(read6));
	read6[1] = 0;
	assert(!holo_fixed_multi_read(read6));
	uint8_t read16[16] = {0x88, 1};
	read16[13] = 3;
	assert(holo_fixed_multi_read(read16));
	read16[13] = 1;
	assert(!holo_fixed_multi_read(read16));
	read16[1] = 0;
	assert(!holo_fixed_multi_read(read16));
	free(mb);
	puts("READ_LEN ring checks passed");
	return 0;
}
