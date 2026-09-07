import pandas as pd
import matplotlib.pyplot as plt
import os
import numpy as np
import mmh3 # MurmurHash3 (하드웨어 해시와 유사, pip install mmh3 필요)
import math
from collections import OrderedDict
from cbf_params import params

from _paths import memory_trace_dir

username = os.path.expanduser('~')

benchmarks = [
    # High-sharing Apps
    'syr2k',
    # 'gups',    
    # 'mm2',
    # 'mm3',
    # 'gemm',
    # 'matrixmultiplication',
    # 'gpt3_gemm',
    # 'llama3_gemm',
    # 'gaussian',
    # 'gramschmidt',
    # 'vgg16',
    # 'lenet',
    # 'spmv',
    # 'mis',
    # 'stencil2d',
    # 'gpt3_mm',
    # 'llama3_mm',
    # 'matrixtranspose',
    # 'im2col',
    # 'bfs',
    # 'kmeans',
    # 'lud',
]

num_chiplets = 36
curr_chip_cols = [str(i) for i in range(36)]

class HLLUnit:
    def __init__(self, b=6):
        self.b = b
        self.m = 1 << b
        self.registers = np.zeros(self.m, dtype=np.uint8)
        self.access_count = 0

    def add(self, addr):
        self.access_count += 1
        # 1. 해시값을 32비트 무부호 정수로 고정 (중요!)
        # signed=False로 하여 음수 방지
        x = mmh3.hash(str(addr), signed=False) & 0xFFFFFFFF
        
        # 2. 버킷 인덱스 추출 (하위 b비트)
        idx = x & (self.m - 1)
        
        # 3. 연속된 0의 개수(rho) 계산 (나머지 상위 비트 사용)
        # x를 b만큼 밀어서 인덱스 비트를 제거
        w = x >> self.b
        
        if w > 0:
            # (남은 비트 전체 길이) - (1이 나타나는 위치) + 1
            # 예: w가 0...0100 이면 bit_length는 3, rho는 (32-6)-3+1 = 24
            rho = (32 - self.b) - w.bit_length() + 1
        else:
            # w가 0이라는 건 상위 비트가 전부 0이라는 뜻 (매우 희귀한 케이스)
            rho = (32 - self.b) + 1
            
        # 4. 기존 값과 비교하여 최대값 갱신
        if rho > self.registers[idx]:
            self.registers[idx] = rho

    def estimate(self):
        m = self.m
        # 빈 버킷 개수 확인 (Linear Counting용)
        v = np.count_nonzero(self.registers == 0)
        
        if v == m: return 0

        # 기본 HLL 추정 공식
        alpha = 0.7213 / (1 + 1.079 / m)
        # 조화 평균 계산: sum(2^-register)
        term_sum = np.sum(1.0 / (2.0 ** self.registers))
        est = alpha * (m**2) / term_sum

        # --- 보정 로직 (이게 핵심) ---
        # 1. 데이터가 적을 때 (Small Range)
        if est <= 2.5 * m:
            if v > 0:
                est = m * math.log(m / v)
        
        # 2. 데이터가 너무 많을 때 (Large Range - 32비트 해시 한계 근접 시)
        # 32비트 해시 기준이므로 2^32 근처에서 보정 필요 (보통 150만 개면 해당 안 됨)
        if est > (1/30) * (2**32):
            est = -(2**32) * math.log(1 - est/(2**32))

        return max(1, int(round(est)))


def simulate_realtime_hll(df, num_entries=1000, bits=4):
    # 하드웨어 리소스: 10개의 HLL 엔트리를 관리하는 LRU 캐시 구조
    hll_table = OrderedDict() 
    
    # 결과 비교를 위해 실제 정답(Ground Truth)을 담을 딕셔너리
    actual_unique_set = {} # {pc: set(addrs)}
    actual_total_access = {} # {pc: count}

    print(f"Starting Real-time Simulation (Entries: {num_entries})...")

    pcs = df['pc'].values
    addrs = df['addr'].values

    for i, row in df.iterrows():
        pc = row['pc']

        requestors = []
        for col in curr_chip_cols:
            if row[col] == 1:
                requestors.append(col)

        # 1. 실제 정답 업데이트 (비교용)
        if pc not in actual_unique_set:
            actual_unique_set[pc] = set()
            actual_total_access[pc] = 0

        for requestor in requestors:
            actual_unique_set[pc].add(requestor)
            actual_total_access[pc] += 1

        # 2. 하드웨어 HLL 유닛 동작
        if pc in hll_table:
            # 기존에 추적 중인 PC면 위치만 최신으로 갱신(LRU) 후 업데이트
            hll_table.move_to_end(pc)
            for requestor in requestors:
                hll_table[pc].add(requestor)
        else:
            # 새로운 PC가 들어왔는데 자리가 없다면 가장 오래된 놈 방출
            if len(hll_table) >= num_entries:
                hll_table.popitem(last=False) 
            
            # 새 HLL 엔트리 할당
            hll_table[pc] = HLLUnit(b=bits)
            for requestor in requestors:
                hll_table[pc].add(requestor)

    # 3. 시뮬레이션 종료 후 결과 비교
    # 현재 하드웨어 테이블에 살아남은 'Top 10' PC들에 대해 검증
    results = []

    for pc, unit in hll_table.items():
        est_unique = unit.estimate()
        real_unique = len(actual_unique_set[pc])
        
        # 0으로 나누는 것을 방지 (최소 1로 취급하거나 0으로 결과 처리)
        hll_reuse = round(unit.access_count / est_unique, 2) if est_unique > 0 else 0
        real_reuse = round(actual_total_access[pc] / real_unique, 2) if real_unique > 0 else 0
        
        # Error Rate 계산 시에도 분모 체크
        error_rate = round(abs(real_unique - est_unique) / real_unique * 100, 2) if real_unique > 0 else 0
        
        results.append({
            'PC': int(pc),
            'Actual_Access': actual_total_access[pc],
            'Actual_Unique': real_unique,
            'HLL_Estimate': est_unique,
            'Error_Rate(%)': error_rate
        })

    return pd.DataFrame(results).sort_values(by='Actual_Access', ascending=False)


sub_dir = "20260329-231025-mesh"
platform = "mesh"
expanded_dir = f'{memory_trace_dir(sub_dir)}/expanded'

results_dir = f'{username}/mgvm/analyze/_out/sketch/hll-results-shared-degree'
os.makedirs(results_dir, exist_ok=True)

for benchmark in benchmarks:
    # if benchmark != "syr2k":
        # continue

    print(f"\n>>> Processing Benchmark: {benchmark}")

    p = params.get(benchmark)
    trace_file = f"{expanded_dir}/{benchmark}_{platform}_6x6_memory_trace.csv"

    if not os.path.exists(trace_file):
        print(f"  Skip: No file {trace_file}")
        continue

    df = pd.read_csv(trace_file, skipinitialspace=True)
    df = df.drop_duplicates()          # full-row 완전-중복 제거
 
    from formatter import get_rdma_df
    rdma_df = get_rdma_df(df)

    b = 6
    print(f" {benchmark}: Simulating HLL with {b} bits for bucket...")
    final_res = simulate_realtime_hll(rdma_df, num_entries=1000, bits=b)
    final_res.to_csv(f"{results_dir}/{benchmark}_bits_{b}.csv", index=False)
        
print(f"\n[!] All simulations finished.")