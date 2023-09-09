package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/perf"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
)

// $BPF_CLANG and $BPF_CFLAGS are set by the Makefile.
//go:generate bpf2go -cc $BPF_CLANG -cflags $BPF_CFLAGS ringbuf ./bpf/http/tc_http.c -type http_data_event -- -I./bpf/headers
//go:generate bpf2go -cc $BPF_CLANG -cflags $BPF_CFLAGS perf ./bpf/http/tc_http_perf.c -type http_data_event -- -I./bpf/headers

const version = "v0.0.1"

var (
	InterfaceName string
	DataPath      string
	Debug         bool
	Verbose       bool
)

func init() {
	flag.StringVar(&InterfaceName, "n", "lo", "a network interface name")
	flag.StringVar(&DataPath, "p", "./db", "a network interface name")
	flag.BoolVar(&Debug, "d", false, "output debug information")
	flag.BoolVar(&Verbose, "v", false, "output more detailed information")
}

func main() {
	flag.Parse()

	if len(InterfaceName) == 0 {
		log.Fatalf("Please specify a network interface")
	}
	// Look up the network interface by name.
	iface, err := net.InterfaceByName(InterfaceName)
	if err != nil {
		log.Fatalf("lookup network iface %s: %s", InterfaceName, err)
	}

	kernelVersion, err := GetKernelVersion()
	if err != nil {
		log.Fatalf("kernel version: NOT OK")
	}
	if !isMinKernelVer(kernelVersion) {
		log.Fatalf("kernel version: NOT OK: minimal supported kernel "+
			"version is %s; kernel version that is running is: %s", minKernelVer, kernelVersion)
	}

	log.Printf("Kernel version: %s", kernelVersion.String())
	log.Printf("  ____       _               ")
	log.Printf(" |  _ \\ _ __(_)___ _ __ ___  ")
	log.Printf(" | |_) | '__| / __| '_ ` _ \\ ")
	log.Printf(" |  __/| |  | \\__ \\ | | | | |")
	log.Printf(" |_|   |_|  |_|___/_| |_| |_|")
	log.Printf("")
	log.Printf("Version %s", version)
	log.Printf("Attached TC program to iface %q (index %d)", iface.Name, iface.Index)
	log.Printf("Press Ctrl-C to exit and remove the program")
	log.Printf("Successfully started! Please run \"sudo cat /sys/kernel/debug/tracing/trace_pipe\" to see output of the BPF programs\n")

	if isMaxKernelVer(kernelVersion) {
		// Load pre-compiled programs into the kernel.
		objs := ringbufObjects{}
		if err := loadRingbufObjects(&objs, nil); err != nil {
			log.Fatalf("loading objects: %s", err)
		}
		defer objs.Close()

		link, err := netlink.LinkByIndex(iface.Index)
		if err != nil {
			log.Fatalf("create net link failed: %v", err)
		}

		infIngress, err := attachTC(link, objs.IngressClsFunc, "classifier/ingress", netlink.HANDLE_MIN_INGRESS)
		if err != nil {
			log.Fatalf("attach tc ingress failed, %v", err)
		}
		defer netlink.FilterDel(infIngress)

		infEgress, err := attachTC(link, objs.EgressClsFunc, "classifier/egress", netlink.HANDLE_MIN_EGRESS)
		if err != nil {
			log.Fatalf("attach tc egress failed, %v", err)
		}
		defer netlink.FilterDel(infEgress)

		// Wait for a signal and close the XDP program,
		stopper := make(chan os.Signal, 1)
		signal.Notify(stopper, os.Interrupt, syscall.SIGTERM)

		rd, err := ringbuf.NewReader(objs.HttpEvents)
		if err != nil {
			log.Fatalf("opening ringbuf reader: %s", err)
		}

		// task queue
		queueTask := make(chan ringbufHttpDataEvent, 100)

		go func() {
			// Wait for a signal and close the ringbuf reader,
			// which will interrupt rd.Read() and make the program exit.
			<-stopper
			close(queueTask)

			if err := rd.Close(); err != nil {
				log.Fatalf("closing perf event reader: %s", err)
			}
		}()

		// run parse,save,query
		runRingBuf(queueTask, rd)
	} else {
		// Load pre-compiled programs into the kernel.
		objs := perfObjects{}
		if err := loadPerfObjects(&objs, nil); err != nil {
			log.Fatalf("loading objects: %s", err)
		}
		defer objs.Close()

		link, err := netlink.LinkByIndex(iface.Index)
		if err != nil {
			log.Fatalf("create net link failed: %v", err)
		}

		infIngress, err := attachTC(link, objs.IngressClsFunc, "classifier/ingress", netlink.HANDLE_MIN_INGRESS)
		if err != nil {
			log.Fatalf("attach tc ingress failed, %v", err)
		}
		defer netlink.FilterDel(infIngress)

		infEgress, err := attachTC(link, objs.EgressClsFunc, "classifier/egress", netlink.HANDLE_MIN_EGRESS)
		if err != nil {
			log.Fatalf("attach tc egress failed, %v", err)
		}
		defer netlink.FilterDel(infEgress)

		// Wait for a signal and close the XDP program,
		stopper := make(chan os.Signal, 1)
		signal.Notify(stopper, os.Interrupt, syscall.SIGTERM)

		// Open a perf event reader from userspace on the PERF_EVENT_ARRAY map
		// described in the eBPF C program.
		rd, err := perf.NewReader(objs.HttpEvents, os.Getpagesize())
		if err != nil {
			log.Fatalf("creating perf event reader: %s", err)
		}
		defer rd.Close()

		// task queue
		queueTask := make(chan perfHttpDataEvent, 100)

		go func() {
			// Wait for a signal and close the ringbuf reader,
			// which will interrupt rd.Read() and make the program exit.
			<-stopper
			close(queueTask)

			if err := rd.Close(); err != nil {
				log.Fatalf("closing perf event reader: %s", err)
			}
		}()

		runPerf(queueTask, rd)
	}

	log.Println("Received signal, exiting TC program..")
}

func runRingBuf(queueTask chan ringbufHttpDataEvent, rd *ringbuf.Reader) {
	log.Printf("Listening for events..")
	db, err := leveldb.OpenFile(DataPath, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	saveChan := make(chan *MergeBuilder, 100)
	go func() {
		for task := range queueTask {
			parseHttp(saveChan, task.Data[:task.DataLen])
		}
	}()

	// save to db
	go saveHttpData(db, saveChan)

	// gin listening
	go runListening(db)

	// bpfHttpDataEventT is generated by bpf2go.
	for {
		var event ringbufHttpDataEvent
		record, err := rd.Read()
		if err != nil {
			if errors.Is(err, perf.ErrClosed) {
				log.Printf("file already closed")
				return
			}
			log.Printf("reading from perf event reader: %s", err)
			continue
		}

		// Parse the perf event entry into a bpfHttpDataEventT structure.
		if err := binary.Read(bytes.NewBuffer(record.RawSample), binary.LittleEndian, &event); err != nil {
			log.Printf("parsing perf event: %s", err)
			continue
		}
		queueTask <- event
	}
}

func runPerf(queueTask chan perfHttpDataEvent, rd *perf.Reader) {
	log.Printf("Listening for events..")
	db, err := leveldb.OpenFile(DataPath, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	saveChan := make(chan *MergeBuilder, 100)
	go func() {
		for task := range queueTask {
			parseHttp(saveChan, task.Data[:task.DataLen])
		}
	}()

	// save to db
	go saveHttpData(db, saveChan)

	// gin listening
	go runListening(db)

	// bpfHttpDataEventT is generated by bpf2go.
	for {
		var event perfHttpDataEvent
		record, err := rd.Read()
		if err != nil {
			if errors.Is(err, perf.ErrClosed) {
				log.Printf("file already closed")
				return
			}
			log.Printf("reading from perf event reader: %s", err)
			continue
		}

		// Parse the perf event entry into a bpfHttpDataEventT structure.
		if err := binary.Read(bytes.NewBuffer(record.RawSample), binary.LittleEndian, &event); err != nil {
			log.Printf("parsing perf event: %s", err)
			continue
		}
		queueTask <- event
	}
}

// replace Qdisc queue
func replaceQdisc(link netlink.Link) error {
	attrs := netlink.QdiscAttrs{
		LinkIndex: link.Attrs().Index,
		Handle:    netlink.MakeHandle(0xffff, 0),
		Parent:    netlink.HANDLE_CLSACT,
	}

	qdisc := &netlink.GenericQdisc{
		QdiscAttrs: attrs,
		QdiscType:  "clsact",
	}

	return netlink.QdiscReplace(qdisc)
}

// attach TC program
func attachTC(link netlink.Link, prog *ebpf.Program, progName string, qdiscParent uint32) (*netlink.BpfFilter, error) {
	if err := replaceQdisc(link); err != nil {
		return nil, fmt.Errorf("replacing clsact qdisc for interface %s: %w", link.Attrs().Name, err)
	}

	filter := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: link.Attrs().Index,
			Parent:    qdiscParent,
			Handle:    1,
			Protocol:  unix.ETH_P_ALL,
			Priority:  1,
		},
		Fd:           prog.FD(),
		Name:         fmt.Sprintf("%s-%s", progName, link.Attrs().Name),
		DirectAction: true,
	}

	if err := netlink.FilterReplace(filter); err != nil {
		return nil, fmt.Errorf("replacing tc filter: %w", err)
	}

	return filter, nil
}
