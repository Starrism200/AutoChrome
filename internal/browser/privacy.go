package browser

import (
	"fmt"
	"hash/fnv"
	"math"
	"strconv"
	"strings"

	"autochrome/internal/fingerprint"
)

func seedUint32(seed string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(seed))
	return h.Sum32()
}

// privacyScript returns deterministic Canvas/Audio noise hooks for one
// container. The same seed always produces the same readback, so a profile
// stays consistent across requests while different containers differ.
func privacyScript(fp fingerprint.Profile, seed string) string {
	canvasDelta := 0
	if !math.IsNaN(fp.CanvasNoise) && !math.IsInf(fp.CanvasNoise, 0) && fp.CanvasNoise > 0 {
		strength := math.Min(fp.CanvasNoise, 1)
		canvasDelta = int(math.Ceil(strength * 3))
		if canvasDelta < 1 {
			canvasDelta = 1
		}
	}
	audioAmp := 0.0
	if !math.IsNaN(fp.AudioNoise) && !math.IsInf(fp.AudioNoise, 0) {
		audioAmp = math.Max(0, math.Min(fp.AudioNoise, 1)) * 1e-5
	}
	if canvasDelta == 0 && audioAmp == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "const acSeed = %d;\n", seedUint32(seed))
	b.WriteString(`function acHash(x, y) {
  let h = (acSeed ^ Math.imul(x | 0, 374761393) ^ Math.imul(y | 0, 668265263)) >>> 0;
  h = Math.imul(h ^ (h >>> 13), 1274126177) >>> 0;
  return (h ^ (h >>> 16)) >>> 0;
}
`)
	if canvasDelta > 0 {
		fmt.Fprintf(&b, "const acCanvasDelta = %d;\n", canvasDelta)
		b.WriteString(`{
  const acOrigGetImageData = CanvasRenderingContext2D.prototype.getImageData;
  const acOrigPutImageData = CanvasRenderingContext2D.prototype.putImageData;
  const acOrigToDataURL = HTMLCanvasElement.prototype.toDataURL;
  const acOrigToBlob = HTMLCanvasElement.prototype.toBlob;
  const acNoiseImageData = function (img) {
    try {
      const d = img.data, w = img.width | 0, max = acCanvasDelta * 2 + 1;
      for (let i = 0; i < d.length; i += 4) {
        const p = i >> 2;
        const x = p % (w || 1), y = (p / (w || 1)) | 0;
        const h = acHash(x, y);
        if ((h % 7) !== 0) continue;
        const delta = (h % max) - acCanvasDelta;
        d[i] = Math.max(0, Math.min(255, d[i] + delta));
        d[i + 1] = Math.max(0, Math.min(255, d[i + 1] + delta));
        d[i + 2] = Math.max(0, Math.min(255, d[i + 2] + delta));
      }
    } catch (e) {}
  };
  const acNoisyCanvas = function (src) {
    const c = document.createElement('canvas');
    c.width = src.width;
    c.height = src.height;
    const x = c.getContext('2d');
    x.drawImage(src, 0, 0);
    const img = acOrigGetImageData.call(x, 0, 0, c.width, c.height);
    acNoiseImageData(img);
    acOrigPutImageData.call(x, img, 0, 0);
    return c;
  };
  CanvasRenderingContext2D.prototype.getImageData = function () {
    const img = acOrigGetImageData.apply(this, arguments);
    acNoiseImageData(img);
    return img;
  };
  HTMLCanvasElement.prototype.toDataURL = function () {
    try { return acOrigToDataURL.apply(acNoisyCanvas(this), arguments); }
    catch (e) { return acOrigToDataURL.apply(this, arguments); }
  };
  HTMLCanvasElement.prototype.toBlob = function () {
    try { return acOrigToBlob.apply(acNoisyCanvas(this), arguments); }
    catch (e) { return acOrigToBlob.apply(this, arguments); }
  };
  if (typeof OffscreenCanvas !== 'undefined') {
    if (typeof OffscreenCanvasRenderingContext2D !== 'undefined' && OffscreenCanvasRenderingContext2D.prototype.getImageData) {
      const acOffGet = OffscreenCanvasRenderingContext2D.prototype.getImageData;
      OffscreenCanvasRenderingContext2D.prototype.getImageData = function () {
        const img = acOffGet.apply(this, arguments);
        acNoiseImageData(img);
        return img;
      };
    }
    const acConvert = OffscreenCanvas.prototype.convertToBlob;
    if (acConvert) {
      OffscreenCanvas.prototype.convertToBlob = function () {
        try {
          const ctx = this.getContext('2d');
          if (ctx) {
            const img = ctx.getImageData(0, 0, this.width, this.height);
            acNoiseImageData(img);
            ctx.putImageData(img, 0, 0);
          }
        } catch (e) {}
        return acConvert.apply(this, arguments);
      };
    }
  }
  const acPatchReadPixels = function (proto) {
    if (!proto || !proto.readPixels) return;
    const orig = proto.readPixels;
    proto.readPixels = function () {
      const r = orig.apply(this, arguments);
      try {
        const pixels = arguments[6];
        if (pixels && pixels.length && pixels.BYTES_PER_ELEMENT === 1) {
          for (let i = 0; i < pixels.length; i += 4) {
            const h = acHash(i, 11);
            if ((h % 11) !== 0) continue;
            const delta = (h % (acCanvasDelta * 2 + 1)) - acCanvasDelta;
            for (let c = 0; c < 3; c++) {
              const v = pixels[i + c] + delta;
              pixels[i + c] = v < 0 ? 0 : v > 255 ? 255 : v;
            }
          }
        }
      } catch (e) {}
      return r;
    };
  };
  acPatchReadPixels(window.WebGLRenderingContext && WebGLRenderingContext.prototype);
  acPatchReadPixels(window.WebGL2RenderingContext && WebGL2RenderingContext.prototype);
}
`)
	}
	if audioAmp > 0 {
		fmt.Fprintf(&b, "const acAudioAmp = %s;\n", strconv.FormatFloat(audioAmp, 'g', -1, 64))
		b.WriteString(`{
  const acAudioNoisy = new WeakSet();
  const acNoiseFloat = function (arr) {
    try {
      for (let i = 0; i < arr.length; i += 97) {
        const h = acHash(i, 19);
        arr[i] += (((h & 0xffff) / 65535) - 0.5) * acAudioAmp;
      }
    } catch (e) {}
  };
  if (typeof AudioBuffer !== 'undefined' && AudioBuffer.prototype.getChannelData) {
    const acOrigChannel = AudioBuffer.prototype.getChannelData;
    AudioBuffer.prototype.getChannelData = function () {
      const data = acOrigChannel.apply(this, arguments);
      if (!acAudioNoisy.has(data)) {
        acNoiseFloat(data);
        acAudioNoisy.add(data);
      }
      return data;
    };
  }
  if (typeof AnalyserNode !== 'undefined') {
    for (const name of ['getFloatFrequencyData', 'getFloatTimeDomainData']) {
      const orig = AnalyserNode.prototype[name];
      if (!orig) continue;
      AnalyserNode.prototype[name] = function (arr) {
        const r = orig.apply(this, arguments);
        acNoiseFloat(arr);
        return r;
      };
    }
    for (const name of ['getByteFrequencyData', 'getByteTimeDomainData']) {
      const orig = AnalyserNode.prototype[name];
      if (!orig) continue;
      AnalyserNode.prototype[name] = function (arr) {
        const r = orig.apply(this, arguments);
        try {
          for (let i = 0; i < arr.length; i += 97) {
            const h = acHash(i, 23);
            const delta = (h % 3) - 1;
            arr[i] = Math.max(0, Math.min(255, arr[i] + delta));
          }
        } catch (e) {}
        return r;
      };
    }
  }
}
`)
	}
	return b.String()
}
