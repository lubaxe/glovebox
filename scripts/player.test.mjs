import {readFileSync} from 'node:fs';
import vm from 'node:vm';
import test from 'node:test';
import assert from 'node:assert/strict';
const context=vm.createContext({window:{}});vm.runInContext(readFileSync(new URL('../internal/web/ui/telemetry.js',import.meta.url),'utf8'),context);
const {at,speed}=context.window.GloveboxTelemetry;
test('Telemetry follows media time, discrete transitions and backwards seeking',()=>{
 const samples=[{time:0,gear:1,brake:false},{time:.5,gear:1,brake:true},{time:1,gear:2,brake:false}];
 assert.equal(at(samples,.4),samples[0]);assert.equal(at(samples,.5),samples[1]);assert.equal(at(samples,1.4),samples[2]);assert.equal(at(samples,.1),samples[0]);
});
test('Missing and stale telemetry never become a fabricated zero-speed state',()=>{assert.equal(at([],0),null);assert.equal(at([{time:1}],0),null);assert.equal(at([{time:1}],2.01),null);});
test('Zero-duration frames choose the newest sample at that media timestamp',()=>{const samples=[{time:0},{time:1,brake:false},{time:1,brake:true}];assert.equal(at(samples,1),samples[2]);});
test('Speed units use metres per second with MPH and km/h conversion',()=>{assert.equal(speed(10,'mph'),22);assert.equal(speed(10,'kmh'),36);assert.equal(speed(0,'mph'),0);});
