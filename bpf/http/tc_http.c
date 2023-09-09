// go:build ignore
#include "vmlinux.h"

#include "bpf_helpers.h"
#include "bpf_tracing.h"
#include "bpf_endian.h"

#define TC_ACT_UNSPEC (-1)
#define TC_ACT_OK 0
#define TC_ACT_SHOT 2
#define TC_ACT_STOLEN 4
#define TC_ACT_REDIRECT 7

#define ETH_P_IP 0x0800 /* Internet Protocol packet        */

#define ETH_HLEN sizeof(struct ethhdr)
#define IP_HLEN sizeof(struct iphdr)
#define TCP_HLEN sizeof(struct tcphdr)
#define UDP_HLEN sizeof(struct udphdr)
#define DNS_HLEN sizeof(struct dns_hdr)

#define HTTP_DATA_MIN_SIZE 91
#define MAX_DATA_SIZE 4000
enum tc_type { Egress, Ingress };

struct http_data_event {
  enum tc_type type;
  __u8 data[MAX_DATA_SIZE];
  __u32 data_len;
};

// BPF ringbuf map
struct {
  __uint(type, BPF_MAP_TYPE_RINGBUF);
  __uint(max_entries, 256 * 1024 /* 256 KB */);
} http_events SEC(".maps");


// BPF programs are limited to a 512-byte stack. We store this value per CPU
// and use it as a heap allocated value.
struct
{
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __type(key, __u32);
    __type(value, struct http_data_event);
    __uint(max_entries, 1);
} data_buffer_heap SEC(".maps");

static __inline struct http_data_event* create_http_data_event() {
  __u32 kZero = 0;
  struct http_data_event* event = bpf_map_lookup_elem(&data_buffer_heap, &kZero);
  if (event == NULL) {
    return NULL;
  }

  return event;
}

static __inline int capture_packets(struct __sk_buff *skb,enum tc_type type) {
    bpf_skb_pull_data(skb, skb->len);
    // Packet data
    void *data_start = (void *)(long)skb->data;
    void *data_end = (void *)(long)skb->data_end;

    // 边界检查：检查数据包是否大于完整以太网 + IP 报头
    if (data_start + ETH_HLEN + IP_HLEN + TCP_HLEN > data_end) {
        return TC_ACT_OK;
    }

    // Ethernet headers
    struct ethhdr *eth = (struct ethhdr *)data_start;
    if (eth->h_proto != bpf_htons(ETH_P_IP)) {
        return TC_ACT_OK;
    }

    // IP headers
    struct iphdr *iph = (struct iphdr *)(data_start + ETH_HLEN);
    if (iph->protocol != IPPROTO_TCP) {
        return TC_ACT_OK;
    }

    __u32 len = (__u32)(data_end-data_start);
    #ifdef DEBUG
        bpf_printk("len: %d\n",len);
    #endif

    if (len < 0) {
        return TC_ACT_OK;
    }

    // In theory this is the minimum packet size of an http packet
    if (len <= HTTP_DATA_MIN_SIZE){
    #ifdef DEBUG
        bpf_printk("---------no http---------\n");
    #endif
        return TC_ACT_OK;
    }

    struct http_data_event* event = create_http_data_event();
    if (event == NULL) {
      return TC_ACT_OK;
    }

    event = bpf_ringbuf_reserve(&http_events, sizeof(struct http_data_event), 0);
    if (!event) {
    #ifdef DEBUG
        bpf_printk("---------no memory---------\n");
    #endif
        return 0;
    }

    event->type = type;
    // This is a max function, but it is written in such a way to keep older BPF verifiers happy.
    event->data_len = (len < MAX_DATA_SIZE ? len : MAX_DATA_SIZE);

    #ifdef DEBUG
        bpf_printk("event->data_len: %d\n",event->data_len);
        bpf_printk("event->data: %d\n",sizeof(event->data));
    #endif

    void *cursor = (void *)(long)skb->data;
    int name_pos = 0;
    for (int i = 0; i < MAX_DATA_SIZE; i++) {
        // boundary judgment
        if (cursor + 1 > data_end) {
        #ifdef DEBUG
          bpf_printk("copy data to boundary");
        #endif
          break;
        }

        event->data[name_pos] = *(char *)(cursor);
        cursor++;
        name_pos++;
    }

    bpf_ringbuf_submit(event, 0);

    return TC_ACT_OK;
}

// egress_cls_func is called for packets that are going out of the network
SEC("classifier/egress")
int egress_cls_func(struct __sk_buff *skb) { return capture_packets(skb,Egress); }

// ingress_cls_func is called for packets that are coming into the network
SEC("classifier/ingress")
int ingress_cls_func(struct __sk_buff *skb) { return capture_packets(skb,Ingress); }

char _license[] SEC("license") = "GPL";
