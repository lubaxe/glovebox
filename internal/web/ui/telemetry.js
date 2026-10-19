'use strict';
// Piecewise-constant recorded values: never interpolate discrete car states
// or carry a stale sample across a gap / minute boundary.
function recordedSampleAt(samples, time) {
  let low=0, high=samples.length;
  while(low<high){const middle=(low+high)>>>1;if(samples[middle].time<=time)low=middle+1;else high=middle;}
  const sample=samples[low-1];
  return sample&&time-sample.time<=1?sample:null;
}
function recordedSpeed(mps, unit) {return Math.round(Math.abs(mps)*(unit==='kmh'?3.6:1/0.44704));}
window.GloveboxTelemetry={at:recordedSampleAt,speed:recordedSpeed};
