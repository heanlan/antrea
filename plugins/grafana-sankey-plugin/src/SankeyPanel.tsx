import React from 'react';
import { PanelProps } from '@grafana/data';
import { SankeyOptions } from 'types';
import Chart from 'react-google-charts';

interface Props extends PanelProps<SankeyOptions> {}

export const SankeyPanel: React.FC<Props> = ({ options, data, width, height }) => {
  let result = [
    ['From', 'To', 'Bytes'],
    ['Source Pod', 'Destination Pod', 0.1],
  ];
  let source = data.series
    .map((series) => series.fields.find((field) => field.name === 'sourcePodName'))
    .map((field) => {
      let record = field?.values as any;
      return record?.buffer;
    })[0];
  if (source !== undefined) {
    let destination = data.series
      .map((series) => series.fields.find((field) => field.name === 'destinationPodName'))
      .map((field) => {
        let record = field?.values as any;
        return record?.buffer;
      })[0];
    let destinationIP = data.series
      .map((series) => series.fields.find((field) => field.name === 'destinationIP'))
      .map((field) => {
        let record = field?.values as any;
        return record?.buffer;
      })[0];
    let bytes = data.series
      .map((series) => series.fields.find((field) => field.name === 'bytes'))
      .map((field) => {
        let record = field?.values as any;
        return record?.buffer;
      })[0];
    let n = source.length;
    for (let i = 0; i < n; i++) {
      let record = [];
      record.push(source[i]);
      if (destination[i] === '') {
        record.push(destinationIP[i]);
      } else {
        record.push(destination[i]);
      }
      record.push(bytes[i]);
      result.push(record);
    }
  }
  return (
    <div>
      <Chart width={600} height={'600px'} chartType="Sankey" loader={<div>Loading Chart</div>} data={result} />
    </div>
  );
};
