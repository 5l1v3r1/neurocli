package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/unixpickle/anydiff"
	"github.com/unixpickle/anydiff/anyseq"
	"github.com/unixpickle/anynet"
	"github.com/unixpickle/anynet/anyff"
	"github.com/unixpickle/anynet/anyrnn"
	"github.com/unixpickle/anynet/anys2s"
	"github.com/unixpickle/anynet/anys2v"
	"github.com/unixpickle/anynet/anysgd"
	"github.com/unixpickle/anyvec"
	"github.com/unixpickle/anyvec/anyvec32"
	"github.com/unixpickle/essentials"
	"github.com/unixpickle/rip"
	"github.com/unixpickle/serializer"
)

func TrainCmd(args []string) {
	var netFile string
	var samplesFile string

	var adam string
	var momentum float64

	var costFunc string
	var stepSize float64
	var batchSize int

	var stopSamples int
	var stopTime time.Duration
	var stopCost float64

	var quiet bool
	var cuda bool

	flags := flag.NewFlagSet("train", flag.ExitOnError)
	flags.StringVar(&netFile, "net", "", "neural network file")
	flags.StringVar(&samplesFile, "samples", "", "training samples file (defaults to stdin)")
	flags.StringVar(&adam, "adam", "", "adam optimizer parameters "+
		`("default" or "RATE1,RATE2")`)
	flags.Float64Var(&momentum, "momentum", 0, "SGD momentum")
	flags.Float64Var(&stepSize, "step", 0.001, "SGD step size")
	flags.IntVar(&batchSize, "batch", 1, "SGD batch size")
	flags.StringVar(&costFunc, "cost", "softmax", "cost function "+
		`("softmax", "mse", or "sigmoidce")`)
	flags.DurationVar(&stopTime, "stoptime", 0, "stop after a `timeout`")
	flags.Float64Var(&stopCost, "stopcost", 0, "stop after cost goes below a `threshold`")
	flags.IntVar(&stopSamples, "stopsamples", 0, "stop after `n` training samples")
	flags.BoolVar(&quiet, "quiet", false, "quiet mode")
	flags.BoolVar(&cuda, "cuda", false, "use CUDA")

	flags.Parse(args)

	if netFile == "" {
		essentials.Die("Missing -net flag. See -help for more.")
	}

	if cuda {
		enableCUDA()
	}

	var net *Network
	if err := serializer.LoadAny(netFile, &net); err != nil {
		essentials.Die("load network:", err)
	}

	reader, err := NewVecReaderFile(samplesFile)
	if err != nil {
		essentials.Die("open samples:", err)
	}

	lastCost, gradienter, fetcher := setupTrainer(net, trainingCostFunc(costFunc),
		reader)

	sgd := &anysgd.SGD{
		Fetcher:     fetcher,
		Gradienter:  gradienter,
		Transformer: trainingTransformer(adam, momentum),
		Samples:     anysgd.LengthSampleList(batchSize),
		Rater:       anysgd.ConstRater(stepSize),
		BatchSize:   batchSize,
	}

	st := newStopper(stopTime, stopCost, stopSamples)
	r := rip.NewRIP()
	sgd.StatusFunc = func(b anysgd.Batch) {
		if st.ShouldStop(*lastCost, sgd.NumProcessed) {
			r.Close()
		}
		if !quiet {
			log.Printf("processed %d samples: cost=%v", sgd.NumProcessed, *lastCost)
		}
	}
	err = sgd.Run(r.Chan())

	if err != nil {
		fmt.Fprintln(os.Stderr, "training error:", err)
	}

	if !quiet {
		log.Println("saving network...")
	}
	if err := serializer.SaveAny(netFile, net); err != nil {
		essentials.Die("save network failed:", err)
	}

	if err != nil {
		os.Exit(1)
	}
}

func trainingCostFunc(name string) anynet.Cost {
	switch name {
	case "softmax":
		return anynet.DotCost{}
	case "mse":
		return anynet.MSE{}
	case "sigmoidce":
		return anynet.SigmoidCE{}
	default:
		essentials.Die("unknown cost function:", name)
	}
	return nil
}

func trainingTransformer(adam string, momentum float64) anysgd.Transformer {
	if adam != "" {
		if momentum != 0 {
			essentials.Die("cannot use Adam and momentum together")
		}
		if adam == "default" {
			return &anysgd.Adam{}
		}
		parts := strings.Split(adam, ",")
		if len(parts) != 2 {
			essentials.Die("bad Adam parameters:", adam)
		}
		rate1, err1 := strconv.ParseFloat(parts[0], 64)
		rate2, err2 := strconv.ParseFloat(parts[1], 64)
		if err1 != nil || err2 != nil {
			essentials.Die("bad Adam parameters:", adam)
		}
		return &anysgd.Adam{DecayRate1: rate1, DecayRate2: rate2}
	} else if momentum != 0 {
		return &anysgd.Momentum{Momentum: momentum}
	} else {
		return nil
	}
}

func setupTrainer(net *Network, c anynet.Cost, r *VecReader) (lastCost *anyvec.Numeric,
	gradienter anysgd.Gradienter, fetcher anysgd.Fetcher) {
	creator := anyvec32.CurrentCreator()
	switch net.Net.(type) {
	case anynet.Layer:
		tr := &anyff.Trainer{
			Net:     net.Net.(anynet.Layer),
			Cost:    c,
			Params:  net.Net.(anynet.Parameterizer).Parameters(),
			Average: true,
		}
		lastCost = &tr.LastCost
		gradienter = tr
		fetcher = &ffFetcher{vr: r, cr: creator, inSize: net.InVecSize,
			outSize: net.OutVecSize}
	case anyrnn.Block, *Bidir:
		tr := &anys2s.Trainer{
			Func: func(s anyseq.Seq) anyseq.Seq {
				if block, ok := net.Net.(anyrnn.Block); ok {
					return anyrnn.Map(s, block)
				} else {
					return net.Net.(*Bidir).Apply(s)
				}
			},
			Cost:    c,
			Params:  net.Net.(anynet.Parameterizer).Parameters(),
			Average: true,
		}
		lastCost = &tr.LastCost
		gradienter = tr
		fetcher = &s2sFetcher{vr: r, cr: creator, inSize: net.InVecSize,
			outSize: net.OutVecSize}
	case *Seq2Vec:
		tr := &anys2v.Trainer{
			Func: func(s anyseq.Seq) anydiff.Res {
				return net.Net.(*Seq2Vec).Apply(s)
			},
			Cost:    c,
			Params:  net.Net.(anynet.Parameterizer).Parameters(),
			Average: true,
		}
		lastCost = &tr.LastCost
		gradienter = tr
		fetcher = &s2vFetcher{vr: r, cr: creator, inSize: net.InVecSize,
			outSize: net.OutVecSize}
	default:
		essentials.Die("unknown network type")
	}
	return
}

type stopper struct {
	timeout <-chan time.Time
	cost    float64
	samples int
}

func newStopper(t time.Duration, cost float64, samples int) *stopper {
	res := &stopper{cost: cost, samples: samples}
	if t != 0 {
		res.timeout = time.After(t)
	}
	return res
}

func (s *stopper) ShouldStop(cost anyvec.Numeric, samples int) bool {
	select {
	case <-s.timeout:
		return true
	default:
	}
	return (s.cost != 0 && cost != nil && float64(cost.(float32)) < s.cost) ||
		(s.samples > 0 && samples > s.samples)
}

type ffFetcher struct {
	vr      *VecReader
	cr      anyvec.Creator
	inSize  int
	outSize int
}

func (f *ffFetcher) Fetch(s anysgd.SampleList) (batch anysgd.Batch, err error) {
	defer essentials.AddCtxTo("fetch samples", &err)
	ins, outs, err := f.vr.ReadSamples(s.Len())
	if err != nil && len(ins) == 0 {
		return nil, err
	}
	var joinedIn, joinedOut []float64
	for i, x := range ins {
		if len(x) != f.inSize {
			return nil, fmt.Errorf("input size should be %d but got %d",
				f.inSize, len(x))
		}
		if len(outs[i]) != f.outSize {
			return nil, fmt.Errorf("output size should be %d but got %d",
				f.outSize, len(outs[i]))
		}
		joinedIn = append(joinedIn, x...)
		joinedOut = append(joinedOut, outs[i]...)
	}
	return &anyff.Batch{
		Inputs:  anydiff.NewConst(f.cr.MakeVectorData(f.cr.MakeNumericList(joinedIn))),
		Outputs: anydiff.NewConst(f.cr.MakeVectorData(f.cr.MakeNumericList(joinedOut))),
		Num:     len(ins),
	}, nil
}

type s2sFetcher struct {
	vr      *VecReader
	cr      anyvec.Creator
	inSize  int
	outSize int
}

func (s *s2sFetcher) Fetch(samples anysgd.SampleList) (batch anysgd.Batch, err error) {
	defer essentials.AddCtxTo("fetch samples", &err)
	ins, outs, err := s.vr.ReadSamples(samples.Len())
	if err != nil && len(ins) == 0 {
		return nil, err
	}
	var inSeqs, outSeqs [][]anyvec.Vector
	for i, joinedIn := range ins {
		in, out, err := s.splitSample(joinedIn, outs[i])
		if err != nil {
			return nil, err
		}
		inSeqs = append(inSeqs, in)
		outSeqs = append(outSeqs, out)
	}
	return &anys2s.Batch{
		Inputs:  anyseq.ConstSeqList(s.cr, inSeqs),
		Outputs: anyseq.ConstSeqList(s.cr, outSeqs),
	}, nil
}

func (s *s2sFetcher) splitSample(in, out []float64) ([]anyvec.Vector, []anyvec.Vector, error) {
	if len(in)%s.inSize != 0 {
		return nil, nil, fmt.Errorf("input not divisible by %d", s.inSize)
	}
	if len(out)%s.outSize != 0 {
		return nil, nil, fmt.Errorf("output not divisible by %d", s.outSize)
	}
	if len(out)/s.outSize != len(in)/s.inSize {
		return nil, nil, fmt.Errorf("sequence length mismatch")
	}
	return splitSeq(s.cr, in, s.inSize), splitSeq(s.cr, out, s.outSize), nil
}

type s2vFetcher struct {
	vr      *VecReader
	cr      anyvec.Creator
	inSize  int
	outSize int
}

func (s *s2vFetcher) Fetch(samples anysgd.SampleList) (batch anysgd.Batch, err error) {
	defer essentials.AddCtxTo("fetch samples", &err)
	ins, outs, err := s.vr.ReadSamples(samples.Len())
	if err != nil && len(ins) == 0 {
		return nil, err
	}
	var inSeqs [][]anyvec.Vector
	var outVecs []anyvec.Vector
	for i, joinedIn := range ins {
		in, out, err := s.splitSample(joinedIn, outs[i])
		if err != nil {
			return nil, err
		}
		inSeqs = append(inSeqs, in)
		outVecs = append(outVecs, out)
	}
	return &anys2v.Batch{
		Inputs:  anyseq.ConstSeqList(s.cr, inSeqs),
		Outputs: anydiff.NewConst(s.cr.Concat(outVecs...)),
	}, nil
}

func (s *s2vFetcher) splitSample(in, out []float64) ([]anyvec.Vector, anyvec.Vector, error) {
	if len(in) == 0 {
		return nil, nil, fmt.Errorf("input must not be empty")
	} else if len(in)%s.inSize != 0 {
		return nil, nil, fmt.Errorf("input not divisible by %d", s.inSize)
	} else if len(out) != s.outSize {
		return nil, nil, fmt.Errorf("output size must be %d", s.outSize)
	}
	outVec := s.cr.MakeVectorData(s.cr.MakeNumericList(out))
	return splitSeq(s.cr, in, s.inSize), outVec, nil
}

func splitSeq(c anyvec.Creator, seq []float64, chunkSize int) []anyvec.Vector {
	var outSeq []anyvec.Vector
	for i := 0; i < len(seq); i += chunkSize {
		v := seq[i : i+chunkSize]
		outSeq = append(outSeq, c.MakeVectorData(c.MakeNumericList(v)))
	}
	return outSeq
}
