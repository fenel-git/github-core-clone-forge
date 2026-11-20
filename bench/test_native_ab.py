"""Unit tests for gates using explicitly synthetic comparison inputs, not benchmark evidence."""
import copy
import unittest
from native_ab import compare, ORDER

def fixtures():
    rows={}
    for label in ORDER:
        candidate=label.startswith('B')
        rows[label]={'trials':[{'trial':i,'state_sha256':'same-test-state','perf':{'perf_p50_us':90 if candidate else 100,'workloads':[{'name':k,'p50_ms':90 if candidate else 100} for k in ('push','clone','read','write')]}} for i in range(3)]}
    return rows
class Gates(unittest.TestCase):
    def test_consistent_win_still_cannot_publish(self):
        r=compare(fixtures());self.assertTrue(r['consistent_composite_win']);self.assertTrue(r['all_states_identical']);self.assertFalse(r['promotion_allowed']);self.assertEqual(r['official_preflight'],'NOT_RUN')
    def test_one_losing_trial_rejects_consistency(self):
        f=fixtures();f['B2']['trials'][1]['perf']['perf_p50_us']=101;self.assertFalse(compare(f)['consistent_composite_win'])
    def test_one_workload_regression_rejected(self):
        f=fixtures();f['B3']['trials'][0]['perf']['workloads'][0]['p50_ms']=101;self.assertFalse(compare(f)['no_workload_regression'])
    def test_changed_export_rejected(self):
        f=fixtures();f['B1']['trials'][2]['state_sha256']='different-test-state';self.assertFalse(compare(f)['all_states_identical'])
    def test_calibration_labeled(self):
        self.assertTrue(compare(fixtures(),True)['calibration_only'])
if __name__=='__main__':unittest.main()
